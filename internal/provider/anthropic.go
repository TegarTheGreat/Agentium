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
// prompt and the latest message so every turn reuses the cached prefix.
type Anthropic struct {
	BaseURL string // e.g. https://api.anthropic.com
	APIKey  string
	Headers map[string]string
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
	Role    string    `json:"role"`
	Content []anBlock `json:"content"`
}

func validArgs(a json.RawMessage) json.RawMessage {
	if len(a) == 0 || !json.Valid(a) {
		return json.RawMessage("{}")
	}
	return a
}

func (c *Anthropic) body(req Request) map[string]any {
	var msgs []anMsg
	push := func(role string, b anBlock) {
		// Consecutive same-role entries (e.g. several tool results) merge
		// into one message, as the API requires alternating roles.
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, b)
			return
		}
		msgs = append(msgs, anMsg{Role: role, Content: []anBlock{b}})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			if m.Text != "" {
				push("user", anBlock{Type: "text", Text: m.Text})
			}
		case RoleAssistant:
			if m.Text != "" {
				push("assistant", anBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				push("assistant", anBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: validArgs(tc.Args)})
			}
		case RoleTool:
			content := m.Text
			if content == "" {
				content = "(empty)"
			}
			push("user", anBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content, IsError: m.IsError})
		}
	}
	if n := len(msgs); n > 0 {
		last := &msgs[n-1].Content[len(msgs[n-1].Content)-1]
		last.CacheControl = ephemeral
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 16000
	}
	b := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTok,
		"messages":   msgs,
		"stream":     true,
	}
	if req.System != "" {
		b["system"] = []anBlock{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Schema}
		}
		tools[len(tools)-1]["cache_control"] = ephemeral
		b["tools"] = tools
	}
	return b
}

type anEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage anUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
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
	h := map[string]string{
		"x-api-key":         c.APIKey,
		"anthropic-version": "2023-06-01",
	}
	for k, v := range c.Headers {
		h[k] = v
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	resp, err := postStream(ctx, base+"/messages", h, c.body(req))
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var out Response
	var text strings.Builder
	type block struct {
		kind, id, name string
		args           strings.Builder
	}
	blocks := map[int]*block{}
	var order []int
	var streamErr error
	done := false
	err = readSSE(resp.Body, func(_, data string) bool {
		var ev anEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			return true
		}
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			out.Usage.Input = u.InputTokens
			out.Usage.CacheRead = u.CacheReadInputTokens
			out.Usage.CacheWrite = u.CacheCreationInputTokens
			out.Usage.Output = u.OutputTokens
		case "content_block_start":
			b := &block{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			blocks[ev.Index] = b
			order = append(order, ev.Index)
		case "content_block_delta":
			b := blocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
				if onText != nil {
					onText(ev.Delta.Text)
				}
			case "input_json_delta":
				if b != nil {
					b.args.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			if ev.Usage.OutputTokens > 0 {
				out.Usage.Output = ev.Usage.OutputTokens
			}
		case "message_stop":
			done = true
			return false
		case "error":
			streamErr = fmt.Errorf("stream error: %s: %s", ev.Error.Type, ev.Error.Message)
			if ev.Error.Type == "overloaded_error" {
				streamErr = &HTTPError{Status: 529, Body: ev.Error.Message}
			}
			return false
		}
		return true
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
	out.Text = text.String()
	for _, i := range order {
		b := blocks[i]
		if b.kind != "tool_use" {
			continue
		}
		args := strings.TrimSpace(b.args.String())
		if args == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.id, Name: b.name, Args: json.RawMessage(args)})
	}
	return out, err
}
