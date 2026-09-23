// Package provider talks to LLM APIs. Most providers speak one of a few
// wire protocols, so Agentium implements protocols, not vendors.
package provider

import (
	"context"
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
}

// Message is one provider-neutral conversation entry.
// A tool result is a RoleTool message with ToolCallID set.
type Message struct {
	Role       Role       `json:"role"`
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
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

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.Input += u2.Input
	u.Output += u2.Output
	u.CacheRead += u2.CacheRead
	u.CacheWrite += u2.CacheWrite
}

// Request is one model call.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int
}

// Response is the assembled result of a streamed model call.
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	Usage      Usage
	StopReason string
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
	var ne net.Error
	if errors.As(err, &ne) {
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
