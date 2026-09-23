package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second}}

// httpTransport speaks MCP Streamable HTTP: every message is a POST; a
// request's reply comes back as JSON or as an SSE stream.
type httpTransport struct {
	c       *Client
	url     string
	headers map[string]string
	tokens  *TokenStore

	mu      sync.Mutex
	session string // Mcp-Session-Id from the initialize reply
	proto   string // negotiated protocol version
}

func (t *httpTransport) send(ctx context.Context, b []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.tokens != nil && req.Header.Get("Authorization") == "" {
		if tok, err := t.tokens.Token(ctx, t.url); err == nil {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	if t.proto != "" {
		req.Header.Set("MCP-Protocol-Version", t.proto)
	}
	t.mu.Unlock()
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		resp.Body.Close()
		return nil
	case resp.StatusCode == http.StatusNotFound && req.Header.Get("Mcp-Session-Id") != "":
		resp.Body.Close()
		return errors.New("MCP session expired (restart agentium to reconnect)")
	case resp.StatusCode == http.StatusUnauthorized && strings.Contains(strings.ToLower(resp.Header.Get("WWW-Authenticate")), "bearer") && t.headers["Authorization"] == "":
		resp.Body.Close()
		return fmt.Errorf("%w (OAuth): run `agentium mcp login %s`", ErrLoginRequired, t.c.Name)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		resp.Body.Close()
		return fmt.Errorf("HTTP %d: check the server's \"headers\" (e.g. Authorization) in config", resp.StatusCode)
	case resp.StatusCode/100 != 2:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The reply (and maybe server requests before it) arrive as SSE
		// events; read them in the background, the caller waits by id.
		go func() {
			defer resp.Body.Close()
			readSSE(resp.Body, func(event, data string) bool {
				if event == "" || event == "message" {
					t.c.dispatch([]byte(data))
				}
				return true
			})
		}()
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) { // a batch
		var msgs []json.RawMessage
		if json.Unmarshal(body, &msgs) == nil {
			for _, m := range msgs {
				t.c.dispatch(m)
			}
		}
		return nil
	}
	t.c.dispatch(body)
	return nil
}

func (t *httpTransport) close() {
	t.mu.Lock()
	sid := t.session
	t.mu.Unlock()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", sid)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// sseTransport is the legacy HTTP+SSE transport (protocol 2024-11-05):
// a GET stream whose first "endpoint" event names the URL to POST to;
// every reply arrives on the stream.
type sseTransport struct {
	endpoint string
	headers  map[string]string
	body     io.ReadCloser
	cancel   context.CancelFunc
}

func startSSE(ctx context.Context, c *Client, cfg Config) (*sseTransport, error) {
	sctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	headers := expand(cfg.Headers)
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("HTTP %d opening the SSE stream", resp.StatusCode)
	}
	t := &sseTransport{headers: headers, body: resp.Body, cancel: cancel}
	endpoint := make(chan string, 1)
	go func() {
		readSSE(resp.Body, func(event, data string) bool {
			switch event {
			case "endpoint":
				select {
				case endpoint <- data:
				default:
				}
			case "", "message":
				c.dispatch([]byte(data))
			}
			return true
		})
		c.shutdown("SSE stream closed")
	}()
	select {
	case ep := <-endpoint:
		base, _ := url.Parse(cfg.URL)
		ref, err := url.Parse(strings.TrimSpace(ep))
		if err != nil {
			t.close()
			return nil, fmt.Errorf("bad endpoint event %q", ep)
		}
		t.endpoint = base.ResolveReference(ref).String()
		return t, nil
	case <-ctx.Done():
		t.close()
		return nil, errors.New("no endpoint event on the SSE stream")
	}
}

func (t *sseTransport) send(ctx context.Context, b []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (t *sseTransport) close() {
	t.cancel()
	t.body.Close()
}

// readSSE calls fn for each event (name, data) until the stream ends.
func readSSE(r io.Reader, fn func(event, data string) bool) {
	br := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data strings.Builder
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && err == nil:
			if data.Len() > 0 && !fn(event, data.String()) {
				return
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line[5:], " "))
		}
		if err != nil {
			if data.Len() > 0 {
				fn(event, data.String())
			}
			return
		}
	}
}
