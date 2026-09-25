package provider

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/models"
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

func TestStreamKeepAliveOnly(t *testing.T) {
	oldIdle, oldProg := StreamIdleTimeout, StreamProgressTimeout
	StreamIdleTimeout, StreamProgressTimeout = 200*time.Millisecond, 400*time.Millisecond
	defer func() { StreamIdleTimeout, StreamProgressTimeout = oldIdle, oldProg }()

	for name, keep := range map[string]string{
		"comment": ": keep-alive\n\n",
		"ping":    "event: ping\ndata: {\"type\":\"ping\"}\n\n",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for {
				fmt.Fprint(w, keep)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}))
		t0 := time.Now()
		var err error
		if name == "ping" {
			_, err = (&Anthropic{BaseURL: srv.URL, APIKey: "k"}).Stream(context.Background(), Request{Model: "m"}, nil)
		} else {
			_, err = (&OpenAI{BaseURL: srv.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
		}
		srv.Close()
		if !errors.Is(err, ErrStalled) || time.Since(t0) > 3*time.Second {
			t.Errorf("%s: %v after %s, want a stall", name, err, time.Since(t0))
		}
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

	// A user cancel is reported as such, never as retryable. (The idle
	// limit is far off here, so a slow machine cannot turn the cancel
	// into a stall.)
	StreamIdleTimeout = 10 * time.Second
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

func TestAnthropicThinkingRoundTrip(t *testing.T) {
	events := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"SIG1\"}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"OPAQUE\"}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"text_delta\",\"text\":\"Reading.\"}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu\",\"name\":\"read\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":3,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a\\\"}\"}}\n\n",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	}
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, events, &body, &hdr)
	defer srv.Close()
	c := &Anthropic{BaseURL: srv.URL, APIKey: "k"}
	resp, err := c.Stream(context.Background(), Request{Model: "claude-opus-5-5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	json.Unmarshal(resp.Raw, &raw)
	if len(raw) != 4 || raw[0]["type"] != "thinking" || raw[0]["signature"] != "SIG1" || raw[0]["thinking"] != "plan" ||
		raw[1]["type"] != "redacted_thinking" || raw[1]["data"] != "OPAQUE" || raw[3]["type"] != "tool_use" {
		t.Fatalf("raw = %s", resp.Raw)
	}
	hist := []Message{
		{Role: RoleUser, Text: "go"},
		{Role: RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls, Raw: resp.Raw, RawModel: "claude-opus-5-5"},
		{Role: RoleTool, ToolCallID: "tu", Text: "data"},
	}
	// Same model: blocks replayed verbatim, in order.
	b, _ := c.body(Request{Model: "claude-opus-5-5", Messages: hist})
	asst := mustJSON(b["messages"].([]anMsg)[1].Content)
	if !strings.Contains(string(asst), "SIG1") || !strings.Contains(string(asst), "OPAQUE") {
		t.Fatalf("thinking not replayed: %s", asst)
	}
	// Other model: rebuilt from text + tool calls, no thinking.
	b, _ = c.body(Request{Model: "claude-sonnet-5", Messages: hist})
	asst = mustJSON(b["messages"].([]anMsg)[1].Content)
	if strings.Contains(string(asst), "SIG1") || !strings.Contains(string(asst), "tool_use") {
		t.Fatalf("cross-model replay: %s", asst)
	}
}

func TestAnthropicReasoningParams(t *testing.T) {
	c := &Anthropic{BaseURL: "https://api.anthropic.com"}
	efforts := []string{"low", "medium", "high", "xhigh", "max"}
	b, betas := c.body(Request{Model: "claude-opus-5-5", Reasoning: Reasoning{Effort: "high", Efforts: efforts}, Fast: true})
	th := b["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["block_binding"] == nil {
		t.Fatalf("thinking = %v", th)
	}
	if b["output_config"].(map[string]any)["effort"] != "high" || b["speed"] != "fast" {
		t.Fatalf("effort/speed: %v %v", b["output_config"], b["speed"])
	}
	if strings.Join(betas, ",") != "thinking-binding-controls-2026-08-01,fast-mode-2026-02-01" {
		t.Fatalf("betas = %v", betas)
	}
	// Sonnet 5: adaptive, no binding controls, no fast mode.
	b, betas = c.body(Request{Model: "claude-sonnet-5", Reasoning: Reasoning{Efforts: efforts}, Fast: true})
	if b["thinking"].(map[string]any)["block_binding"] != nil || b["speed"] != nil || len(betas) != 0 || b["output_config"] != nil {
		t.Fatalf("sonnet 5 body: %v %v", b, betas)
	}
	// Budget-only model (Haiku 4.5): budget thinking only when effort asked.
	b, _ = c.body(Request{Model: "claude-haiku-4-5", Reasoning: Reasoning{Budget: true}})
	if b["thinking"] != nil {
		t.Fatal("no effort: no thinking")
	}
	b, _ = c.body(Request{Model: "claude-haiku-4-5", MaxTokens: 4000, Reasoning: Reasoning{Budget: true, Effort: "high"}})
	if b["thinking"].(map[string]any)["budget_tokens"] != 16000 || b["max_tokens"].(int) <= 16000 {
		t.Fatalf("budget: %v max=%v", b["thinking"], b["max_tokens"])
	}
	// Compatible endpoints get no Anthropic-only beta features.
	c2 := &Anthropic{BaseURL: "https://proxy.example"}
	b, betas = c2.body(Request{Model: "claude-opus-5-5", Reasoning: Reasoning{Efforts: efforts}, Fast: true, Tools: sampleReq.Tools})
	if len(betas) != 0 || b["speed"] != nil || strings.Contains(string(mustJSON(b["tools"])), "eager") {
		t.Fatalf("proxy body: %v %v", b, betas)
	}
}

func TestOpenAIReasoningAndExtras(t *testing.T) {
	events := []string{
		`data: {"choices":[{"delta":{"reasoning_content":"think "}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"reasoning_content":"more"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{}"},"extra_content":{"google":{"thought_signature":"TS"}}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	var body map[string]any
	srv := sseServer(t, events, &body, nil)
	defer srv.Close()
	c := &OpenAI{BaseURL: srv.URL}
	resp, err := c.Stream(context.Background(), Request{Model: "deepseek-v4-flash"}, nil)
	if err != nil || resp.Reasoning != "think more" || !strings.Contains(string(resp.ToolCalls[0].Extra), "TS") {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	hist := []Message{{Role: RoleUser, Text: "x"},
		{Role: RoleAssistant, ToolCalls: resp.ToolCalls, Reasoning: resp.Reasoning, RawModel: "deepseek-v4-flash"},
		{Role: RoleTool, ToolCallID: "c1", Text: "ok"}}
	b := c.body(Request{Model: "deepseek-v4-flash", Messages: hist, MaxTokens: 100,
		Reasoning: Reasoning{Effort: "xhigh", Efforts: []string{"low", "high", "max"}, Interleaved: "reasoning_content"}})
	j := string(mustJSON(b))
	for _, want := range []string{`"reasoning_content":"think more"`, `"thought_signature":"TS"`, `"reasoning_effort":"high"`, `"max_tokens":100`} {
		if !strings.Contains(j, want) {
			t.Errorf("body missing %s: %s", want, j)
		}
	}
	// Official OpenAI uses max_completion_tokens.
	b = (&OpenAI{BaseURL: "https://api.openai.com/v1"}).body(Request{Model: "gpt-5.6", MaxTokens: 100})
	if b["max_completion_tokens"] != 100 || b["max_tokens"] != nil {
		t.Fatalf("official body %v", b)
	}
}

func TestClosestEffort(t *testing.T) {
	if ClosestEffort("xhigh", []string{"low", "medium", "high"}) != "high" || ClosestEffort("max", nil) != "max" || ClosestEffort("low", []string{"minimal", "low"}) != "low" {
		t.Fatal("ClosestEffort")
	}
}

type stub struct {
	err   error
	calls int
	model string
}

func (s *stub) Stream(_ context.Context, req Request, _ func(string)) (Response, error) {
	s.calls++
	s.model = req.Model
	if s.err != nil {
		return Response{}, s.err
	}
	return Response{Text: "from " + req.Model}, nil
}

func TestFallback(t *testing.T) {
	a := &stub{err: &HTTPError{Status: 529}}
	b := &stub{}
	swaps := 0
	f := &Fallback{Chain: []Resolved{{Model: "main", Client: a}, {Model: "backup", Client: b}}, OnSwap: func(_, _ Resolved, _ error) { swaps++ }}
	resp, err := f.Stream(context.Background(), Request{Model: "ignored"}, nil)
	if err != nil || resp.Text != "from backup" || swaps != 1 || f.Active().Model != "backup" {
		t.Fatalf("resp=%+v err=%v swaps=%d", resp, err, swaps)
	}
	// Sticky: the next request goes straight to the backup.
	f.Stream(context.Background(), Request{}, nil)
	if a.calls != 1 || b.calls != 2 {
		t.Fatalf("calls a=%d b=%d", a.calls, b.calls)
	}
	// After the recovery time the first model gets another chance.
	f.Recover = time.Nanosecond
	a.err = nil
	time.Sleep(time.Millisecond)
	if resp, _ := f.Stream(context.Background(), Request{}, nil); resp.Text != "from main" || f.Active().Model != "main" {
		t.Fatalf("did not return to the first model: %+v", resp)
	}
	// Non-retryable errors are returned without trying others.
	c := &stub{err: &HTTPError{Status: 400}}
	d := &stub{}
	f2 := &Fallback{Chain: []Resolved{{Model: "x", Client: c}, {Model: "y", Client: d}}}
	if _, err := f2.Stream(context.Background(), Request{}, nil); err == nil || d.calls != 0 {
		t.Fatal("400 must not fall back")
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := map[error]bool{
		&net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}: false,
		&net.DNSError{Err: "timeout", Name: "x", IsTimeout: true}:               true,
		x509.UnknownAuthorityError{}:                                            false,
		&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}:                     true,
		fmt.Errorf("wrap: %w", io.ErrUnexpectedEOF):                             true,
		&HTTPError{Status: 404}:                                                 false,
		context.Canceled:                                                        false,
	}
	for err, want := range cases {
		if got := Retryable(err); got != want {
			t.Errorf("Retryable(%T %v) = %v, want %v", err, err, got, want)
		}
	}
}

func TestImagesInRequests(t *testing.T) {
	png := Image{MediaType: "image/png", Data: []byte("PNGDATA")}
	hist := []Message{
		{Role: RoleUser, Text: "what is in this?", Images: []Image{png}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "a", Name: "read", Args: json.RawMessage(`{}`)}, {ID: "b", Name: "read", Args: json.RawMessage(`{}`)}}},
		{Role: RoleTool, ToolCallID: "a", Text: "(image a.png — attached)", Images: []Image{png}},
		{Role: RoleTool, ToolCallID: "b", Text: "text only"},
	}
	ab, _ := (&Anthropic{}).body(Request{Model: "claude-x", Messages: hist})
	js, _ := json.Marshal(ab["messages"])
	s := string(js)
	b64 := base64.StdEncoding.EncodeToString(png.Data)
	if !strings.Contains(s, `{"source":{"data":"`+b64+`","media_type":"image/png","type":"base64"},"type":"image"}`) {
		t.Fatalf("anthropic user image missing: %s", s)
	}
	if !strings.Contains(s, `"content":[{"type":"text","text":"(image a.png — attached)"}`) || !strings.Contains(s, `"tool_use_id":"a"`) {
		t.Fatalf("anthropic tool_result image missing: %s", s)
	}

	ob := (&OpenAI{}).body(Request{Model: "gpt-5", Messages: hist})
	msgs := ob["messages"].([]oaMsg)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,tool,tool,user" {
		t.Fatalf("openai roles: %v", roles)
	}
	js, _ = json.Marshal(msgs)
	if n := strings.Count(string(js), `"url":"data:image/png;base64,`+b64+`"`); n != 2 {
		t.Fatalf("openai images = %d: %s", n, js)
	}
}

func TestVision(t *testing.T) {
	cases := []struct {
		r    Resolved
		want bool
	}{
		{Resolved{Model: "claude-sonnet-5"}, true},
		{Resolved{Model: "deepseek-chat"}, false},
		{Resolved{Model: "o3-mini"}, false},
		{Resolved{Model: "custom", Known: true, Info: models.Model{Input: []string{"text", "image"}}}, true},
		{Resolved{Model: "gpt-5", Known: true, Info: models.Model{Input: []string{"text"}}}, false},
	}
	for _, c := range cases {
		if got := c.r.Vision(); got != c.want {
			t.Errorf("%s: Vision()=%v", c.r.Model, got)
		}
	}
}

func TestResolveProviderOnly(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "k")
	r, err := Resolve("deepseek", config.Config{}, config.Auth{})
	if err != nil || r.Provider != "deepseek" || r.Model != "deepseek-flash" {
		t.Fatalf("got %s/%s, %v", r.Provider, r.Model, err)
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte(`{"data":[{"id":"m-b"},{"id":"m-a"}]}`))
	}))
	defer srv.Close()
	cfg := config.Config{Providers: map[string]config.ProviderConf{"x": {Protocol: "openai", BaseURL: srv.URL + "/v1", APIKeyEnv: "X_KEY"}}}
	t.Setenv("X_KEY", "k")
	ids, err := ListModels(context.Background(), "x", cfg, config.Auth{})
	if err != nil || strings.Join(ids, ",") != "m-a,m-b" {
		t.Fatalf("%v %v", ids, err)
	}
}

func TestOpenAIToolCallsWithoutIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"read","arguments":"{\"path\""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":":\"x\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"read","arguments":"{\"path\":\"y\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", d)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := &OpenAI{BaseURL: srv.URL}
	resp, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil || len(resp.ToolCalls) != 2 || string(resp.ToolCalls[0].Args) != `{"path":"x"}` || string(resp.ToolCalls[1].Args) != `{"path":"y"}` {
		t.Fatalf("%+v %v", resp.ToolCalls, err)
	}
}

func TestAnthropicBodySafety(t *testing.T) {
	c := &Anthropic{}
	req := Request{Model: "claude-x", Reasoning: Reasoning{Effort: "high", Budget: true}, Messages: []Message{
		{Role: RoleUser, Text: "hi"},
		{Role: RoleAssistant, Text: "  \n", ToolCalls: []ToolCall{{ID: "functions.read:0", Name: "read", Args: json.RawMessage(`{}`)}}},
		{Role: RoleTool, ToolCallID: "functions.read:0", Text: "ok"},
	}}
	b, _ := c.body(req)
	raw, _ := json.Marshal(b)
	s := string(raw)
	if strings.Contains(s, `"thinking":{`) {
		t.Error("thinking enabled for a tool round without a thinking block")
	}
	if strings.Contains(s, "functions.read:0") || !strings.Contains(s, "functions_read_0") {
		t.Error("tool id not made valid")
	}
	if strings.Contains(s, `"text":"  \n"`) {
		t.Error("whitespace-only text block sent")
	}
	req.MaxOutput = 8192
	req.Messages = req.Messages[:1]
	b, _ = c.body(req)
	if mt := b["max_tokens"].(int); mt > 8192 {
		t.Errorf("max_tokens %d above the model's output limit", mt)
	}
}

func TestCloseToolCalls(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Text: "go"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: RoleTool, ToolCallID: "a", Text: "ok"},
	}
	out := CloseToolCalls(msgs)
	if len(out) != 4 || out[3].ToolCallID != "b" || !out[3].IsError {
		t.Fatalf("%+v", out)
	}
	if len(CloseToolCalls(out)) != 4 {
		t.Fatal("a closed history changes")
	}
}

func TestErrorBodyWith200(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"You exceeded your current quota"}}`,
		"event: error\ndata: {\"message\":\"You exceeded your current quota\"}\n\n",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		_, err := (&OpenAI{BaseURL: srv.URL}).Stream(context.Background(), Request{Model: "m"}, nil)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "exceeded your current quota") || Retryable(err) {
			t.Fatalf("%q: %v (retryable %v)", body, err, Retryable(err))
		}
	}
}
