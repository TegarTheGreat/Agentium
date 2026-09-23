package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Anthropic speaks the Messages protocol (Anthropic API and compatible
// endpoints). It sets prompt-cache breakpoints on the tools, the system
// prompt and the latest message so every turn reuses the cached prefix,
// and replays signed thinking blocks unchanged as the API requires.
type Anthropic struct {
	BaseURL string // e.g. https://api.anthropic.com
	APIKey  string
	Headers map[string]string
	// URL, if set, is the full endpoint (Vertex, Bedrock proxies); the
	// model then travels in the URL, not the body, and Version is sent
	// as anthropic_version in the body.
	URL     string
	Version string
	// Bearer sends the key as "Authorization: Bearer" instead of x-api-key.
	Bearer bool
	// Token, if set, supplies a fresh bearer token per request.
	Token func(context.Context) (string, error)
}

var ephemeral = map[string]string{"type": "ephemeral"}

type anBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        json.RawMessage   `json:"input,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      string            `json:"content,omitempty"`
	IsError      bool              `json:"is_error,omitempty"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type anMsg struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

func validArgs(a json.RawMessage) json.RawMessage {
	if len(a) == 0 || !json.Valid(a) {
		return json.RawMessage("{}")
	}
	return a
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// official reports whether requests go to Anthropic's own API, where beta
// features like fast mode and thinking-binding controls exist.
func (c *Anthropic) official() bool {
	return c.URL == "" && (c.BaseURL == "" || strings.Contains(c.BaseURL, "api.anthropic.com"))
}

// PreservedThinking reports models whose thinking blocks are bound to the
// exact conversation prefix (history edits invalidate them). Older
// models' signatures cover only the block itself.
func PreservedThinking(model string) bool {
	for _, m := range []string{"opus-5-5", "fable-5-1", "mythos-5-1"} {
		if strings.Contains(model, m) {
			return true
		}
	}
	return false
}

// fastCapable lists models with a fast output mode on the Claude API.
func fastCapable(model string) bool {
	for _, m := range []string{"claude-opus-5-5", "claude-opus-5", "claude-opus-4-8"} {
		if model == m || strings.HasPrefix(model, m+"-") {
			return true
		}
	}
	return false
}

func (c *Anthropic) body(req Request) (map[string]any, []string) {
	var msgs []anMsg
	push := func(role string, b json.RawMessage) {
		// Consecutive same-role entries (e.g. several tool results) merge
		// into one message, as the API requires alternating roles.
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, b)
			return
		}
		msgs = append(msgs, anMsg{Role: role, Content: []json.RawMessage{b}})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			if m.Text != "" {
				push("user", mustJSON(anBlock{Type: "text", Text: m.Text}))
			}
		case RoleAssistant:
			if len(m.Raw) > 0 && m.RawModel == req.Model {
				var blocks []json.RawMessage
				if json.Unmarshal(m.Raw, &blocks) == nil && len(blocks) > 0 {
					for _, b := range blocks {
						push("assistant", b)
					}
					continue
				}
			}
			if m.Text != "" {
				push("assistant", mustJSON(anBlock{Type: "text", Text: m.Text}))
			}
			for _, tc := range m.ToolCalls {
				push("assistant", mustJSON(anBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: validArgs(tc.Args)}))
			}
		case RoleTool:
			content := m.Text
			if content == "" {
				content = "(empty)"
			}
			push("user", mustJSON(anBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content, IsError: m.IsError}))
		}
	}
	// Cache breakpoint on the newest block (it must not be a thinking block).
	for i := len(msgs) - 1; i >= 0; i-- {
		last := msgs[i].Content[len(msgs[i].Content)-1]
		var obj map[string]json.RawMessage
		if json.Unmarshal(last, &obj) == nil {
			var typ string
			_ = json.Unmarshal(obj["type"], &typ)
			if typ != "thinking" && typ != "redacted_thinking" {
				obj["cache_control"] = mustJSON(ephemeral)
				msgs[i].Content[len(msgs[i].Content)-1] = mustJSON(obj)
			}
		}
		break
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 32000
	}
	b := map[string]any{
		"max_tokens": maxTok,
		"messages":   msgs,
		"stream":     true,
	}
	if c.URL == "" {
		b["model"] = req.Model
	}
	if c.Version != "" {
		b["anthropic_version"] = c.Version
	}
	if req.System != "" {
		b["system"] = []anBlock{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}
	var betas []string
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Schema}
			if c.official() {
				// Stream large tool inputs (file contents) as they are generated.
				tools[i]["eager_input_streaming"] = true
			}
		}
		tools[len(tools)-1]["cache_control"] = ephemeral
		b["tools"] = tools
	}
	r := req.Reasoning
	switch {
	case len(r.Efforts) > 0: // adaptive-thinking models (4.6+)
		th := map[string]any{"type": "adaptive"}
		if c.official() && PreservedThinking(req.Model) {
			// If history was edited (elision/compaction), drop the now-stale
			// thinking blocks instead of failing the request.
			th["block_binding"] = map[string]any{"prefix_mismatch_behavior": "drop_block"}
			betas = append(betas, "thinking-binding-controls-2026-08-01")
		}
		b["thinking"] = th
		if r.Effort != "" {
			b["output_config"] = map[string]any{"effort": ClosestEffort(r.Effort, r.Efforts)}
		}
	case r.Budget && r.Effort != "":
		budget := map[string]int{"minimal": 1024, "low": 2048, "medium": 8000, "high": 16000, "xhigh": 24000, "max": 32000}[r.Effort]
		if budget == 0 {
			budget = 8000
		}
		if maxTok <= budget {
			b["max_tokens"] = budget + 8000
		}
		b["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	if req.Fast && c.official() && fastCapable(req.Model) {
		b["speed"] = "fast"
		betas = append(betas, "fast-mode-2026-02-01")
	}
	return b, betas
}

type anEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage anUsage `json:"usage"`
	} `json:"message"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage anUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// Stream implements Client.
func (c *Anthropic) Stream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	h := map[string]string{"anthropic-version": "2023-06-01"}
	if c.Version != "" {
		delete(h, "anthropic-version") // travels in the body instead
	}
	key := c.APIKey
	if c.Token != nil {
		t, err := c.Token(ctx)
		if err != nil {
			return Response{}, err
		}
		key = t
	}
	if c.Bearer || c.Token != nil {
		h["Authorization"] = "Bearer " + key
	} else {
		h["x-api-key"] = key
	}
	for k, v := range c.Headers {
		h[k] = v
	}
	body, betas := c.body(req)
	if len(betas) > 0 {
		if cur := h["anthropic-beta"]; cur != "" {
			betas = append([]string{cur}, betas...)
		}
		h["anthropic-beta"] = strings.Join(betas, ",")
	}
	url := c.URL
	if url == "" {
		base := strings.TrimRight(c.BaseURL, "/")
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		url = base + "/messages"
	}
	resp, err := postStream(ctx, url, h, body)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	return decodeAnthropic(ctx, resp.Body, onText)
}

// block accumulates one streamed content block.
type anPartial struct {
	start json.RawMessage
	kind  string
	id    string
	name  string
	args  strings.Builder
	text  strings.Builder
	think strings.Builder
	sig   string
}

func decodeAnthropic(ctx context.Context, r interface{ Read([]byte) (int, error) }, onText func(string)) (Response, error) {
	var out Response
	blocks := map[int]*anPartial{}
	var order []int
	var streamErr error
	done := false
	err := readSSE(r, func(_, data string) bool {
		var ev anEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
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
	a := assembleAnthropic(blocks, order)
	out.Text, out.ToolCalls, out.Raw = a.Text, a.ToolCalls, a.Raw
	return out, err
}

// assembleAnthropic turns streamed blocks into text, tool calls and the
// raw content array (signed thinking blocks included) for replay.
func assembleAnthropic(blocks map[int]*anPartial, order []int) Response {
	var out Response
	var text strings.Builder
	var raw []json.RawMessage
	for _, i := range order {
		b := blocks[i]
		switch b.kind {
		case "text":
			text.WriteString(b.text.String())
			raw = append(raw, mustJSON(map[string]any{"type": "text", "text": b.text.String()}))
		case "tool_use":
			args := strings.TrimSpace(b.args.String())
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.id, Name: b.name, Args: json.RawMessage(args)})
			raw = append(raw, mustJSON(map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": validArgs(json.RawMessage(args))}))
		case "thinking":
			raw = append(raw, mustJSON(map[string]any{"type": "thinking", "thinking": b.think.String(), "signature": b.sig}))
		default:
			// redacted_thinking and unknown block types replay verbatim.
			raw = append(raw, b.start)
		}
	}
	out.Text = text.String()
	if len(raw) > 0 {
		out.Raw = mustJSON(raw)
	}
	return out
}

func handleAnthropicEvent(out *Response, ev anEvent, blocks map[int]*anPartial, order *[]int, onText func(string), streamErr *error, done *bool) bool {
	switch ev.Type {
	case "message_start":
		u := ev.Message.Usage
		out.Usage.Input = u.InputTokens
		out.Usage.CacheRead = u.CacheReadInputTokens
		out.Usage.CacheWrite = u.CacheCreationInputTokens
		out.Usage.Output = u.OutputTokens
	case "content_block_start":
		var cb struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		}
		_ = json.Unmarshal(ev.ContentBlock, &cb)
		b := &anPartial{start: ev.ContentBlock, kind: cb.Type, id: cb.ID, name: cb.Name}
		b.text.WriteString(cb.Text)
		b.think.WriteString(cb.Thinking)
		if cb.Text != "" && onText != nil {
			onText(cb.Text)
		}
		blocks[ev.Index] = b
		*order = append(*order, ev.Index)
	case "content_block_delta":
		b := blocks[ev.Index]
		if b == nil {
			return true
		}
		switch ev.Delta.Type {
		case "text_delta":
			b.text.WriteString(ev.Delta.Text)
			if onText != nil {
				onText(ev.Delta.Text)
			}
		case "input_json_delta":
			b.args.WriteString(ev.Delta.PartialJSON)
		case "thinking_delta":
			b.think.WriteString(ev.Delta.Thinking)
		case "signature_delta":
			b.sig += ev.Delta.Signature
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			out.StopReason = ev.Delta.StopReason
		}
		if ev.Usage.OutputTokens > 0 {
			out.Usage.Output = ev.Usage.OutputTokens
		}
	case "message_stop":
		*done = true
		return false
	case "error":
		*streamErr = fmt.Errorf("stream error: %s: %s", ev.Error.Type, ev.Error.Message)
		if ev.Error.Type == "overloaded_error" {
			*streamErr = &HTTPError{Status: 529, Body: ev.Error.Message}
		}
		return false
	}
	return true
}
