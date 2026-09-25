// Package provider talks to LLM APIs. Most providers speak one of a few
// wire protocols, so Agentium implements protocols, not vendors.
package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"
)

// Role of a message in the conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a model request to run a tool.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	// Extra is provider data that must be sent back with the call
	// (e.g. Gemini's thought signature in extra_content).
	Extra json.RawMessage `json:"extra,omitempty"`
	// BadArgs holds arguments that were not valid JSON (Args is then {}
	// so the history stays sendable and saveable); the call is answered
	// with an error instead of run.
	BadArgs string `json:"-"`
}

// Message is one provider-neutral conversation entry.
// A tool result is a RoleTool message with ToolCallID set.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text,omitempty"`
	// Typed is what the user typed, for a message that starts a turn
	// (Text may carry context and notes around it); At is when it was
	// sent. Neither is sent to the model.
	Typed      string     `json:"typed,omitempty"`
	At         time.Time  `json:"at,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`

	// Raw is the provider-native content of an assistant message (for
	// Anthropic: the content blocks, including signed thinking blocks,
	// which must be replayed unchanged). RawModel is the model that
	// produced it; Raw is only replayed to that same model. Code that
	// edits history must clear Raw on the affected messages.
	Raw      json.RawMessage `json:"raw,omitempty"`
	RawModel string          `json:"raw_model,omitempty"`
	// Reasoning is visible reasoning text some OpenAI-compatible models
	// return (reasoning_content) and need back on later turns.
	Reasoning string `json:"reasoning,omitempty"`
	// ReasoningField is where Reasoning came from, to send it back the
	// same way when the registry does not say.
	ReasoningField string `json:"reasoning_field,omitempty"`
	// Images attached to a user message or a tool result.
	Images []Image `json:"images,omitempty"`
}

// Image is an inline image (PNG, JPEG, GIF or WebP).
type Image struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

// DataURL returns the image as a data: URL.
func (im Image) DataURL() string {
	return "data:" + im.MediaType + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
}

// ToolDef describes a tool to the model. Schema is a JSON Schema object.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Usage counts tokens for one model call.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// Minus returns u - u2 (usage since an earlier snapshot).
func (u Usage) Minus(u2 Usage) Usage {
	return Usage{Input: u.Input - u2.Input, Output: u.Output - u2.Output,
		CacheRead: u.CacheRead - u2.CacheRead, CacheWrite: u.CacheWrite - u2.CacheWrite}
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.Input += u2.Input
	u.Output += u2.Output
	u.CacheRead += u2.CacheRead
	u.CacheWrite += u2.CacheWrite
}

// Reasoning configures thinking for one request. Capabilities come from
// the model registry; zero values mean "provider default".
type Reasoning struct {
	Effort      string   // low | medium | high | xhigh | max (or minimal) — "" = model default
	Efforts     []string // efforts the model accepts; non-empty means adaptive-capable
	Budget      bool     // model only supports budget_tokens thinking
	Interleaved string   // assistant field that carries reasoning back (e.g. reasoning_content)
}

// Request is one model call.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int
	// MaxOutput is the model's output limit (0 if unknown): max_tokens and
	// thinking budgets stay below it.
	MaxOutput int
	Reasoning Reasoning
	// Fast asks for the provider's fast output mode when available.
	Fast bool
}

// Response is the assembled result of a streamed model call.
type Response struct {
	// ReasoningField is the delta field reasoning arrived in
	// (OpenAI-compatible: "reasoning_content" or "reasoning").
	ReasoningField string
	Text           string
	ToolCalls      []ToolCall
	Usage          Usage
	StopReason     string
	Raw            json.RawMessage // provider-native assistant content, see Message.Raw
	Reasoning      string
	// Thought is the model's thinking in readable form, for display only
	// (Claude's thinking blocks travel in Raw).
	Thought string
	// Model is the model that actually served the reply when it differs
	// from the request (fallback); "" means the requested model.
	Model string
}

// ClosestEffort maps want onto the values a model supports.
func ClosestEffort(want string, supported []string) string {
	if want == "" || len(supported) == 0 {
		return want
	}
	order := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	rank := func(v string) int {
		for i, o := range order {
			if o == v {
				return i
			}
		}
		return -1
	}
	best, bestD := supported[0], 99
	w := rank(want)
	for _, s := range supported {
		if s == want {
			return s
		}
		d := rank(s) - w
		if d < 0 {
			d = -d
		}
		if rank(s) >= 0 && d < bestD {
			best, bestD = s, d
		}
	}
	return best
}

// Client streams one model call. onText receives text deltas as they arrive
// and may be nil.
type Client interface {
	Stream(ctx context.Context, req Request, onText func(string)) (Response, error)
}

// HTTPError is a non-2xx API response.
type HTTPError struct {
	Status int
	Body   string
	// RetryAfter is the server's requested wait, if it sent one.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("api error %d: %s", e.Status, e.Body)
}

// ErrStalled means the stream sent nothing for too long.
var ErrStalled = errors.New("stream stalled")

// ErrIncomplete means the stream ended before the model finished.
var ErrIncomplete = errors.New("stream ended early")

// Retryable reports whether err is worth retrying: rate limits, overload,
// 5xx, and broken or stalled connections. Client errors (4xx) are not.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == 429 || he.Status == 408 || he.Status == 529 || he.Status >= 500
	}
	if errors.Is(err, ErrStalled) || errors.Is(err, ErrIncomplete) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// Permanent network failures: bad certificates, unknown hosts.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	var certErr *tls.CertificateVerificationError
	var authErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &authErr) || errors.As(err, &hostErr) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) { // server closed the connection before replying
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "stream error") && strings.Contains(msg, "INTERNAL_ERROR")
}

// RetryAfter returns the server-requested delay carried by err, if any.
func RetryAfter(err error) time.Duration {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.RetryAfter
	}
	return 0
}

// NextEffort returns the next reasoning level above cur that the model
// supports, or "" when there is none (or the model has no levels). An
// empty cur counts as "high", the usual default.
func (r Reasoning) NextEffort() string {
	order := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	supported := r.Efforts
	if len(supported) == 0 {
		if !r.Budget || r.Effort == "" {
			// No levels, or thinking is off on a budget_tokens model:
			// switching it on mid tool-loop is not allowed by the API.
			return ""
		}
		supported = order // budget_tokens models: any level maps to a budget
	}
	cur := r.Effort
	if cur == "" {
		cur = "high"
	}
	at := -1
	for i, o := range order {
		if o == cur {
			at = i
		}
	}
	for _, o := range order[at+1:] {
		for _, s := range supported {
			if s == o {
				return o
			}
		}
	}
	return ""
}

// UserWords is what the user typed in a user message: without the
// context blocks agentium puts before it (<recall>, <hook-context>,
// <session-start>, mentioned files).
func UserWords(s string) string {
	for {
		t := strings.TrimLeft(s, " \n")
		found := false
		for _, tag := range []string{"recall", "hook-context", "session-start", "mentioned-file", "mentioned-dir"} {
			if strings.HasPrefix(t, "<"+tag) {
				if j := strings.Index(t, "</"+tag+">"); j >= 0 {
					s, found = t[j+len("</"+tag+">"):], true
				}
			}
		}
		if !found {
			return strings.TrimSpace(s)
		}
	}
}

// CloseToolCalls returns msgs with an answer for every tool call: a call
// left without its result (the process stopped mid-round) gets one saying
// so, since providers reject a history with unanswered calls.
func CloseToolCalls(msgs []Message) []Message {
	var out []Message
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		out = append(out, m)
		if m.Role != RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for i+1 < len(msgs) && msgs[i+1].Role == RoleTool {
			i++
			out = append(out, msgs[i])
			answered[msgs[i].ToolCallID] = true
		}
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				out = append(out, Message{Role: RoleTool, ToolCallID: c.ID, Text: "[not finished: agentium was stopped before this call completed]", IsError: true})
			}
		}
	}
	return out
}
