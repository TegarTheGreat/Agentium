package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
)

func sseServer(t *testing.T, events []string, gotBody *map[string]any, gotHeader *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if gotBody != nil {
			_ = json.Unmarshal(b, gotBody)
		}
		if gotHeader != nil {
			*gotHeader = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprint(w, e)
			w.(http.Flusher).Flush()
		}
	}))
}

var sampleReq = Request{
	Model:  "m",
	System: "sys",
	Messages: []Message{
		{Role: RoleUser, Text: "hi"},
		{Role: RoleAssistant, Text: "checking", ToolCalls: []ToolCall{
			{ID: "a", Name: "read", Args: json.RawMessage(`{"path":"x"}`)},
			{ID: "b", Name: "read", Args: json.RawMessage(`{"path":"y"}`)},
		}},
		{Role: RoleTool, ToolCallID: "a", Text: "X"},
		{Role: RoleTool, ToolCallID: "b", Text: "boom", IsError: true},
	},
	Tools: []ToolDef{{Name: "read", Description: "r", Schema: json.RawMessage(`{"type":"object"}`)}},
}

func TestOpenAIStream(t *testing.T) {
	events := []string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"cmd\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"read","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, events, &body, &hdr)
	defer srv.Close()

	c := &OpenAI{BaseURL: srv.URL + "/v1", APIKey: "k"}
	var streamed strings.Builder
	resp, err := c.Stream(context.Background(), sampleReq, func(d string) { streamed.WriteString(d) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Hello" || streamed.String() != "Hello" {
		t.Fatalf("text = %q streamed %q", resp.Text, streamed.String())
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "bash" || string(resp.ToolCalls[0].Args) != `{"cmd":"ls"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if string(resp.ToolCalls[1].Args) != "{}" {
		t.Fatalf("empty args should become {}, got %s", resp.ToolCalls[1].Args)
	}
	if resp.Usage != (Usage{Input: 40, Output: 7, CacheRead: 60}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if hdr.Get("Authorization") != "Bearer k" {
		t.Fatalf("auth header = %q", hdr.Get("Authorization"))
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 5 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
	asst := msgs[2].(map[string]any)
	if len(asst["tool_calls"].([]any)) != 2 {
		t.Fatalf("assistant tool calls missing: %v", asst)
	}
	if msgs[4].(map[string]any)["tool_call_id"] != "b" {
		t.Fatalf("tool message = %v", msgs[4])
	}
}

func TestAnthropicStream(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":900,\"cache_creation_input_tokens\":5,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"On it\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu1\",\"name\":\"edit\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"a.txt\\\",\\\"new\\\":\\\"x\\\"}\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu2\",\"name\":\"search\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":42}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, events, &body, &hdr)
	defer srv.Close()

	c := &Anthropic{BaseURL: srv.URL, APIKey: "k"}
	resp, err := c.Stream(context.Background(), sampleReq, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "On it" || resp.StopReason != "tool_use" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 2 || string(resp.ToolCalls[0].Args) != `{"path":"a.txt","new":"x"}` || string(resp.ToolCalls[1].Args) != "{}" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Usage != (Usage{Input: 12, Output: 42, CacheRead: 900, CacheWrite: 5}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if hdr.Get("x-api-key") != "k" || hdr.Get("anthropic-version") == "" {
		t.Fatalf("headers = %v", hdr)
	}
	// Two tool results must merge into one user message; cache breakpoints
	// on system, last tool and last message block.
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 alternating messages, got %d: %v", len(msgs), msgs)
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if len(last) != 2 || last[1].(map[string]any)["is_error"] != true {
		t.Fatalf("tool results = %v", last)
	}
	if last[1].(map[string]any)["cache_control"] == nil {
		t.Fatal("missing cache_control on last block")
	}
	if body["system"].([]any)[0].(map[string]any)["cache_control"] == nil {
		t.Fatal("missing cache_control on system")
	}
	tools := body["tools"].([]any)
	if tools[len(tools)-1].(map[string]any)["cache_control"] == nil {
		t.Fatal("missing cache_control on tools")
	}
}

func TestAnthropicStreamError(t *testing.T) {
	events := []string{
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
	}
	srv := sseServer(t, events, nil, nil)
	defer srv.Close()
	_, err := (&Anthropic{BaseURL: srv.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil || !Retryable(err) {
		t.Fatalf("want retryable error, got %v", err)
	}
}

func TestHTTPErrorRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate"}`, 429)
	}))
	defer srv.Close()
	_, err := (&OpenAI{BaseURL: srv.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
	if !Retryable(err) || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v", err)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", 401)
	}))
	defer srv2.Close()
	_, err = (&OpenAI{BaseURL: srv2.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil || Retryable(err) {
		t.Fatalf("401 must not be retryable: %v", err)
	}
}

func TestResolve(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY", "OLLAMA_HOST"} {
		t.Setenv(k, "")
	}
	cfg := config.Config{Providers: map[string]config.ProviderConf{
		"corp": {BaseURL: "https://llm.corp/v1", APIKeyEnv: "CORP_KEY"},
	}}
	auth := config.Auth{"openai": {APIKey: "sk-stored"}}

	if _, err := Resolve("", cfg, config.Auth{}); err == nil {
		t.Fatal("expected error with no credentials")
	}
	r, err := Resolve("", cfg, auth)
	if err != nil || r.Provider != "openai" {
		t.Fatalf("default = %+v %v", r, err)
	}
	if r.Client.(*OpenAI).APIKey != "sk-stored" {
		t.Fatal("stored key not used")
	}
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	r, err = Resolve("claude-haiku-4-5", cfg, auth)
	if err != nil || r.Provider != "anthropic" || r.Model != "claude-haiku-4-5" {
		t.Fatalf("guess = %+v %v", r, err)
	}
	r, err = Resolve("openrouter/anthropic/claude-sonnet-5", cfg, config.Auth{"openrouter": {APIKey: "x"}})
	if err != nil || r.Provider != "openrouter" || r.Model != "anthropic/claude-sonnet-5" {
		t.Fatalf("nested model = %+v %v", r, err)
	}
	r, err = Resolve("ollama/qwen3", cfg, config.Auth{})
	if err != nil || r.Client.(*OpenAI).BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("ollama = %+v %v", r, err)
	}
	if _, err = Resolve("corp/big", cfg, config.Auth{}); err == nil || !strings.Contains(err.Error(), "CORP_KEY") {
		t.Fatalf("custom provider without key: %v", err)
	}
	t.Setenv("CORP_KEY", "ck")
	r, err = Resolve("corp/big", cfg, config.Auth{})
	if err != nil || r.Client.(*OpenAI).BaseURL != "https://llm.corp/v1" {
		t.Fatalf("custom = %+v %v", r, err)
	}
	if _, err = Resolve("mystery-model", cfg, auth); err == nil {
		t.Fatal("expected unknown provider error")
	}
}

func TestOpenAIStreamOptionsFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "stream_options") {
			http.Error(w, `{"error":"unknown field stream_options"}`, 400)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := &OpenAI{BaseURL: srv.URL}
	for i := 0; i < 2; i++ {
		resp, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
		if err != nil || resp.Text != "ok" {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (one rejected, then remembered)", calls)
	}
}

func TestStreamStallAndIncomplete(t *testing.T) {
	old := StreamIdleTimeout
	StreamIdleTimeout = 200 * time.Millisecond
	defer func() { StreamIdleTimeout = old }()

	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer stall.Close()
	t0 := time.Now()
	_, err := (&OpenAI{BaseURL: stall.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
	if !errors.Is(err, ErrStalled) || !Retryable(err) || time.Since(t0) > 2*time.Second {
		t.Fatalf("stall: %v after %s", err, time.Since(t0))
	}

	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
	}))
	defer cut.Close()
	_, err = (&Anthropic{BaseURL: cut.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
	if !errors.Is(err, ErrIncomplete) || !Retryable(err) {
		t.Fatalf("incomplete: %v", err)
	}

	// A user cancel is reported as such, never as retryable.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err = (&OpenAI{BaseURL: stall.URL}).Stream(ctx, Request{Model: "m"}, nil)
	if !errors.Is(err, context.Canceled) || Retryable(err) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "slow down", 429)
	}))
	defer srv.Close()
	_, err := (&OpenAI{BaseURL: srv.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
	if RetryAfter(err) != 7*time.Second {
		t.Fatalf("retry-after = %v", RetryAfter(err))
	}
	if parseRetryAfter("garbage") != 0 || parseRetryAfter("1.5") != 1500*time.Millisecond {
		t.Fatal("parseRetryAfter")
	}
}
