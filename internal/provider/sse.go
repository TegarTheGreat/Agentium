package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StreamIdleTimeout aborts a stream that sends nothing for this long.
// Reasoning models can pause between events, so it is generous.
var StreamIdleTimeout = 120 * time.Second

// httpClient has no overall timeout: streams can run for minutes. Callers
// cancel through the request context.
var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		ForceAttemptHTTP2:     true,
	},
}

// idleBody cancels the request when no bytes arrive for the idle period.
type idleBody struct {
	io.ReadCloser
	timer   *time.Timer
	idle    time.Duration
	cancel  context.CancelFunc
	mu      sync.Mutex
	stalled bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && err != io.EOF {
		b.mu.Lock()
		stalled := b.stalled
		b.mu.Unlock()
		if stalled {
			return n, ErrStalled
		}
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// postStream POSTs body as JSON and returns the response when it is 2xx.
// The returned body aborts with ErrStalled if the stream goes quiet.
func postStream(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b)), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	ib := &idleBody{ReadCloser: resp.Body, idle: StreamIdleTimeout, cancel: cancel}
	ib.timer = time.AfterFunc(StreamIdleTimeout, func() {
		ib.mu.Lock()
		ib.stalled = true
		ib.mu.Unlock()
		cancel()
	})
	resp.Body = ib
	return resp, nil
}

// readSSE calls fn for each event with its event name and data payload.
// Returning false from fn stops reading.
func readSSE(r io.Reader, fn func(event, data string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var event string
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 {
			event = ""
			return true
		}
		ok := fn(event, data.String())
		event = ""
		data.Reset()
		return ok
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line[5:], " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
