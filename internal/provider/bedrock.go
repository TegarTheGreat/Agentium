package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// AWSCreds are static AWS credentials.
type AWSCreds struct {
	AccessKey, SecretKey, SessionToken string
}

// AWSCredsFromEnv reads the standard AWS environment variables.
func AWSCredsFromEnv() (AWSCreds, bool) {
	c := AWSCreds{os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")}
	return c, c.AccessKey != "" && c.SecretKey != ""
}

// Bedrock runs Anthropic models on Amazon Bedrock through
// InvokeModelWithResponseStream. It authenticates with a Bedrock API key
// (bearer) or signs requests with SigV4.
type Bedrock struct {
	Region string
	Bearer string // AWS_BEARER_TOKEN_BEDROCK
	Creds  AWSCreds
	// Endpoint overrides https://bedrock-runtime.<region>.amazonaws.com.
	Endpoint string
	now      func() time.Time
}

func (b *Bedrock) endpoint() string {
	if b.Endpoint != "" {
		return strings.TrimRight(b.Endpoint, "/")
	}
	return "https://bedrock-runtime." + b.Region + ".amazonaws.com"
}

// Stream implements Client.
func (b *Bedrock) Stream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	an := &Anthropic{URL: "bedrock", Version: "bedrock-2023-05-31"}
	body, betas := an.body(req)
	delete(body, "stream")
	if len(betas) > 0 {
		body["anthropic_beta"] = betas
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	// Send the model id percent-encoded (":" → %3A) like the AWS SDKs do,
	// so the path we sign is the path the server sees.
	path := "/model/" + req.Model + "/invoke-with-response-stream"
	rawPath := "/model/" + awsURIEncode(req.Model, true) + "/invoke-with-response-stream"
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint()+rawPath, bytes.NewReader(buf))
	if err != nil {
		return Response{}, err
	}
	hreq.URL.Path, hreq.URL.RawPath = path, rawPath
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/vnd.amazon.eventstream")
	if b.Bearer != "" {
		hreq.Header.Set("Authorization", "Bearer "+b.Bearer)
	} else {
		now := time.Now
		if b.now != nil {
			now = b.now
		}
		signV4(hreq, buf, b.Region, "bedrock", b.Creds, now())
	}
	resp, err := httpClient.Do(hreq)
	if err != nil {
		return Response{}, err
	}
	watchIdle(resp, cancel)
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Response{}, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(msg)), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	var out Response
	blocks := map[int]*anPartial{}
	var order []int
	var streamErr error
	done := false
	err = readEventStream(resp.Body, func(headers map[string]string, payload []byte) bool {
		if headers[":message-type"] == "exception" || headers[":message-type"] == "error" {
			kind := headers[":exception-type"]
			streamErr = fmt.Errorf("bedrock %s: %s", kind, strings.TrimSpace(string(payload)))
			if kind == "throttlingException" || kind == "serviceUnavailableException" || kind == "internalServerException" || kind == "modelStreamErrorException" {
				streamErr = &HTTPError{Status: 503, Body: string(payload)}
			}
			return false
		}
		var chunk struct {
			Bytes string `json:"bytes"`
		}
		if json.Unmarshal(payload, &chunk) != nil || chunk.Bytes == "" {
			return true
		}
		data, err := base64.StdEncoding.DecodeString(chunk.Bytes)
		if err != nil {
			return true
		}
		var ev anEvent
		if json.Unmarshal(data, &ev) != nil {
			return true
		}
		return handleAnthropicEvent(&out, ev, blocks, &order, onText, &streamErr, &done)
	})
	if err == nil {
		err = streamErr
	}
	if err == nil && !done && out.StopReason == "" {
		err = ErrIncomplete
	}
	if err != nil && ctx.Err() != nil && !errors.Is(err, ErrStalled) {
		err = ctx.Err()
	}
	// Reuse the Anthropic assembler for text/tool calls/raw blocks.
	assembled := assembleAnthropic(blocks, order)
	out.Text, out.ToolCalls, out.Raw = assembled.Text, assembled.ToolCalls, assembled.Raw
	return out, err
}

// readEventStream decodes AWS event-stream frames:
// total len (4) | headers len (4) | prelude crc (4) | headers | payload | crc (4).
func readEventStream(r io.Reader, fn func(headers map[string]string, payload []byte) bool) error {
	prelude := make([]byte, 12)
	for {
		if _, err := io.ReadFull(r, prelude); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		total := binary.BigEndian.Uint32(prelude[0:4])
		hlen := binary.BigEndian.Uint32(prelude[4:8])
		if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
			return errors.New("eventstream: bad prelude checksum")
		}
		if total < 16 || total > 16<<20 || hlen > total-16 {
			return errors.New("eventstream: bad frame length")
		}
		rest := make([]byte, total-12)
		if _, err := io.ReadFull(r, rest); err != nil {
			return err
		}
		msgCRC := binary.BigEndian.Uint32(rest[len(rest)-4:])
		if crc32.Update(crc32.ChecksumIEEE(prelude), crc32.IEEETable, rest[:len(rest)-4]) != msgCRC {
			return errors.New("eventstream: bad message checksum")
		}
		headers := parseESHeaders(rest[:hlen])
		if !fn(headers, rest[hlen:len(rest)-4]) {
			return nil
		}
	}
}

func parseESHeaders(b []byte) map[string]string {
	h := map[string]string{}
	for len(b) > 0 {
		n := int(b[0])
		if len(b) < 1+n+1 {
			break
		}
		name := string(b[1 : 1+n])
		typ := b[1+n]
		b = b[2+n:]
		skip := func(n int) bool {
			if len(b) < n {
				return false
			}
			b = b[n:]
			return true
		}
		switch typ {
		case 7: // string
			if len(b) < 2 {
				return h
			}
			l := int(binary.BigEndian.Uint16(b))
			if len(b) < 2+l {
				return h
			}
			h[name] = string(b[2 : 2+l])
			b = b[2+l:]
		case 0, 1: // bool true/false
		case 2:
			if !skip(1) {
				return h
			}
		case 3:
			if !skip(2) {
				return h
			}
		case 4:
			if !skip(4) {
				return h
			}
		case 5, 8:
			if !skip(8) {
				return h
			}
		case 6: // bytes
			if len(b) < 2 || !skip(2+int(binary.BigEndian.Uint16(b))) {
				return h
			}
		case 9: // uuid
			if !skip(16) {
				return h
			}
		default:
			return h
		}
	}
	return h
}

// encodeEventStream builds one frame (used by tests).
func encodeEventStream(headers map[string]string, payload []byte) []byte {
	var hb bytes.Buffer
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		hb.WriteByte(byte(len(k)))
		hb.WriteString(k)
		hb.WriteByte(7)
		binary.Write(&hb, binary.BigEndian, uint16(len(headers[k])))
		hb.WriteString(headers[k])
	}
	total := uint32(12 + hb.Len() + len(payload) + 4)
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, total)
	binary.Write(&out, binary.BigEndian, uint32(hb.Len()))
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(out.Bytes()[:8]))
	out.Write(hb.Bytes())
	out.Write(payload)
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(out.Bytes()))
	return out.Bytes()
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// awsURIEncode encodes a path per SigV4 (RFC 3986 unreserved kept).
func awsURIEncode(s string, encodeSlash bool) string {
	var sb strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			sb.WriteByte(c)
		case c == '/' && !encodeSlash:
			sb.WriteByte(c)
		default:
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// canonicalHook lets tests inspect the canonical request.
var canonicalHook func(string)

// signV4 adds AWS Signature Version 4 headers to req.
func signV4(req *http.Request, body []byte, region, service string, c AWSCreds, t time.Time) {
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	payloadHash := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	if service != "service" { // the AWS test suite omits this header
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	host := req.URL.Host
	names := []string{"host"}
	vals := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" || lk == "accept" || lk == "content-length" {
			continue
		}
		names = append(names, lk)
		vals[lk] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + vals[n] + "\n")
	}
	signed := strings.Join(names, ";")
	// Canonical path: each segment URI-encoded once more (non-S3 services).
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
		segs[i] = awsURIEncode(awsURIEncode(s, true), true)
	}
	canonPath := strings.Join(segs, "/")
	q := req.URL.Query()
	qk := make([]string, 0, len(q))
	for k := range q {
		qk = append(qk, k)
	}
	sort.Strings(qk)
	var cq []string
	for _, k := range qk {
		for _, v := range q[k] {
			cq = append(cq, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
		}
	}
	canonical := strings.Join([]string{req.Method, canonPath, strings.Join(cq, "&"), canonHeaders.String(), signed, payloadHash}, "\n")
	if canonicalHook != nil {
		canonicalHook(canonical)
	}
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	k := hmacSHA256([]byte("AWS4"+c.SecretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}
