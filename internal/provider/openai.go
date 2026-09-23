package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// OpenAI speaks the Chat Completions protocol, which most providers
// (OpenAI, OpenRouter, Groq, Cerebras, DeepSeek, xAI, Mistral, Gemini's
// compat endpoint, Ollama, LM Studio, vLLM, ...) accept.
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Headers map[string]string

	noUsageOpt atomic.Bool // endpoint rejected stream_options
}

type oaMsg struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"` // *string, or []oaPart with images
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	// Reasoning replay for models that need it (DeepSeek, Kimi, ...).
	ReasoningContent *string `json:"reasoning_content,omitempty"`
	Reasoning        *string `json:"reasoning,omitempty"`
}

type oaPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	ImageURL map[string]string `json:"image_url,omitempty"`
}

func oaParts(text string, images []Image) []oaPart {
	parts := []oaPart{{Type: "text", Text: text}}
	for _, im := range images {
		parts = append(parts, oaPart{Type: "image_url", ImageURL: map[string]string{"url": im.DataURL()}})
	}
	return parts
}

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	// ExtraContent carries provider data such as Gemini thought
	// signatures, which must be returned with the call.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content          string       `json:"content"`
			ReasoningContent string       `json:"reasoning_content"`
			Reasoning        string       `json:"reasoning"`
			ToolCalls        []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func strp(s string) *string { return &s }

func (c *OpenAI) body(req Request) map[string]any {
	msgs := make([]oaMsg, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaMsg{Role: "system", Content: strp(req.System)})
	}
	var toolImages []Image
	for i, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			if len(m.Images) > 0 {
				msgs = append(msgs, oaMsg{Role: "user", Content: oaParts(m.Text, m.Images)})
				continue
			}
			msgs = append(msgs, oaMsg{Role: "user", Content: strp(m.Text)})
		case RoleAssistant:
			om := oaMsg{Role: "assistant"}
			if m.Text != "" || len(m.ToolCalls) == 0 {
				om.Content = strp(m.Text)
			}
			for _, tc := range m.ToolCalls {
				var otc oaToolCall
				otc.ID, otc.Type = tc.ID, "function"
				otc.Function.Name = tc.Name
				otc.Function.Arguments = string(tc.Args)
				otc.ExtraContent = tc.Extra
				om.ToolCalls = append(om.ToolCalls, otc)
			}
			if m.Reasoning != "" && m.RawModel == req.Model {
				switch req.Reasoning.Interleaved {
				case "reasoning_content":
					om.ReasoningContent = strp(m.Reasoning)
				case "reasoning":
					om.Reasoning = strp(m.Reasoning)
				}
			}
			msgs = append(msgs, om)
		case RoleTool:
			msgs = append(msgs, oaMsg{Role: "tool", Content: strp(m.Text), ToolCallID: m.ToolCallID})
			// Tool messages carry text only; images follow in a user
			// message once the run of tool results ends.
			toolImages = append(toolImages, m.Images...)
			if len(toolImages) > 0 && (i+1 == len(req.Messages) || req.Messages[i+1].Role != RoleTool) {
				msgs = append(msgs, oaMsg{Role: "user", Content: oaParts("(images returned by the tool calls above)", toolImages)})
				toolImages = nil
			}
		}
	}
	b := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	}
	if !c.noUsageOpt.Load() {
		b["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxTokens > 0 {
		if c.official() {
			b["max_completion_tokens"] = req.MaxTokens // OpenAI's reasoning models reject max_tokens
		} else {
			b["max_tokens"] = req.MaxTokens
		}
	}
	if r := req.Reasoning; r.Effort != "" && len(r.Efforts) > 0 {
		b["reasoning_effort"] = ClosestEffort(r.Effort, r.Efforts)
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Schema,
			}}
		}
		b["tools"] = tools
	}
	return b
}

func (c *OpenAI) official() bool { return strings.Contains(c.BaseURL, "api.openai.com") }

// Stream implements Client.
func (c *OpenAI) Stream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	h := map[string]string{}
	if c.APIKey != "" {
		h["Authorization"] = "Bearer " + c.APIKey
	}
	for k, v := range c.Headers {
		h[k] = v
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	resp, err := postStream(ctx, url, h, c.body(req))
	var he *HTTPError
	if errors.As(err, &he) && he.Status == 400 && strings.Contains(he.Body, "stream_options") && !c.noUsageOpt.Load() {
		// Some compatible servers reject stream_options; retry without it.
		c.noUsageOpt.Store(true)
		resp, err = postStream(ctx, url, h, c.body(req))
	}
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var out Response
	var text strings.Builder
	type partial struct {
		id, name string
		args     strings.Builder
		extra    json.RawMessage
	}
	var reasoning strings.Builder
	calls := map[int]*partial{}
	var streamErr error
	done := false
	err = readSSE(resp.Body, func(_, data string) bool {
		if data == "[DONE]" {
			done = true
			return false
		}
		var ch oaChunk
		if json.Unmarshal([]byte(data), &ch) != nil {
			return true
		}
		if ch.Error != nil {
			streamErr = fmt.Errorf("stream error: %s", ch.Error.Message)
			return false
		}
		if ch.Usage != nil {
			out.Usage = Usage{
				Input:     ch.Usage.PromptTokens - ch.Usage.PromptTokensDetails.CachedTokens,
				Output:    ch.Usage.CompletionTokens,
				CacheRead: ch.Usage.PromptTokensDetails.CachedTokens,
			}
		}
		for _, choice := range ch.Choices {
			reasoning.WriteString(choice.Delta.ReasoningContent)
			reasoning.WriteString(choice.Delta.Reasoning)
			if d := choice.Delta.Content; d != "" {
				text.WriteString(d)
				if onText != nil {
					onText(d)
				}
			}
			for i, tc := range choice.Delta.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				p := calls[idx]
				if p == nil {
					p = &partial{}
					calls[idx] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				if len(tc.ExtraContent) > 0 {
					p.extra = tc.ExtraContent
				}
				p.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != "" {
				out.StopReason = choice.FinishReason
			}
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
	out.Reasoning = reasoning.String()
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for n, i := range idxs {
		p := calls[i]
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%d", n)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: id, Name: p.name, Args: json.RawMessage(args), Extra: p.extra})
	}
	return out, err
}
