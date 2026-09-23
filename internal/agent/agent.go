// Package agent runs the model/tool loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/tool"
)

// Events lets the UI observe the loop. Any field may be nil.
type Events struct {
	Text       func(delta string)
	ToolStart  func(call provider.ToolCall)
	ToolDone   func(call provider.ToolCall, out string, err error, d time.Duration)
	Retry      func(err error, wait time.Duration)
	TurnFinish func(resp provider.Response)
}

// Agent holds one conversation.
type Agent struct {
	Client    provider.Client
	Model     string
	System    string
	Tools     []tool.Tool
	Env       *tool.Env
	MaxTurns  int
	MaxTokens int
	// ContextChars is the soft budget for the conversation, in characters.
	// Past it, old tool outputs are elided in one batch (rarely, so the
	// provider's prompt cache survives most turns).
	ContextChars int
	Events       Events

	Messages []provider.Message
	Usage    provider.Usage
	Turns    int
	// Note is prepended to the next user input once (e.g. "the user undid
	// your last changes"), so the model's picture of the files stays true.
	Note string
}

// Stats summarizes one Run.
type Stats struct {
	Turns     int
	ToolCalls int
	Usage     provider.Usage
	Elapsed   time.Duration
}

// ErrMaxTurns is returned when the loop hits MaxTurns.
var ErrMaxTurns = errors.New("stopped: reached max turns")

// Run sends input and loops until the model stops calling tools.
func (a *Agent) Run(ctx context.Context, input string) (Stats, error) {
	start := time.Now()
	var st Stats
	if a.Note != "" {
		input = "[" + a.Note + "]\n\n" + input
		a.Note = ""
	}
	if a.Env != nil {
		a.Env.StartTurn()
	}
	a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser, Text: input})
	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 100
	}
	defs := tool.Defs(a.Tools)
	for st.Turns < maxTurns {
		a.elide()
		resp, err := a.call(ctx, provider.Request{
			Model: a.Model, System: a.System, Messages: a.Messages, Tools: defs, MaxTokens: a.MaxTokens,
		})
		st.Turns++
		a.Turns++
		st.Usage.Add(resp.Usage)
		a.Usage.Add(resp.Usage)
		if err != nil {
			// Keep whatever text streamed so the conversation stays coherent.
			if resp.Text != "" {
				a.Messages = append(a.Messages, provider.Message{Role: provider.RoleAssistant, Text: resp.Text})
			}
			st.Elapsed = time.Since(start)
			return st, err
		}
		a.Messages = append(a.Messages, provider.Message{Role: provider.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		if a.Events.TurnFinish != nil {
			a.Events.TurnFinish(resp)
		}
		if len(resp.ToolCalls) == 0 {
			st.Elapsed = time.Since(start)
			return st, nil
		}
		st.ToolCalls += len(resp.ToolCalls)
		results := a.runTools(ctx, resp.ToolCalls)
		a.Messages = append(a.Messages, results...)
		if ctx.Err() != nil {
			st.Elapsed = time.Since(start)
			return st, ctx.Err()
		}
	}
	st.Elapsed = time.Since(start)
	return st, ErrMaxTurns
}

// call streams one model call, retrying rate limits, overloads and broken
// or stalled connections. Text already streamed to the user may repeat
// after a mid-stream retry; the Retry event lets the UI say so.
func (a *Agent) call(ctx context.Context, req provider.Request) (provider.Response, error) {
	var resp provider.Response
	var err error
	for attempt := 0; ; attempt++ {
		resp, err = a.Client.Stream(ctx, req, a.Events.Text)
		if err == nil || !provider.Retryable(err) || ctx.Err() != nil || attempt >= maxRetries {
			return resp, err
		}
		wait := time.Duration(1<<attempt) * time.Second
		if ra := provider.RetryAfter(err); ra > wait {
			wait = ra
		}
		if wait > maxRetryWait {
			wait = maxRetryWait
		}
		if a.Events.Retry != nil {
			a.Events.Retry(err, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return resp, ctx.Err()
		}
	}
}

const (
	maxRetries   = 4
	maxRetryWait = 60 * time.Second
)

// runTools executes calls in parallel and returns results in call order.
func (a *Agent) runTools(ctx context.Context, calls []provider.ToolCall) []provider.Message {
	byName := make(map[string]tool.Tool, len(a.Tools))
	for _, t := range a.Tools {
		byName[t.Def.Name] = t
	}
	out := make([]provider.Message, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c provider.ToolCall) {
			defer wg.Done()
			if a.Events.ToolStart != nil {
				a.Events.ToolStart(c)
			}
			t0 := time.Now()
			var res string
			var err error
			t, ok := byName[c.Name]
			switch {
			case !ok:
				err = fmt.Errorf("unknown tool %q", c.Name)
			case len(c.Args) > 0 && !json.Valid(c.Args):
				err = errors.New("arguments are not valid JSON")
			default:
				res, err = safeRun(ctx, t, a.Env, c.Args)
			}
			if a.Events.ToolDone != nil {
				a.Events.ToolDone(c, res, err, time.Since(t0))
			}
			msg := provider.Message{Role: provider.RoleTool, ToolCallID: c.ID, Text: res}
			if err != nil {
				msg.IsError = true
				if res != "" {
					msg.Text = res + "\nerror: " + err.Error()
				} else {
					msg.Text = "error: " + err.Error()
				}
			}
			out[i] = msg
		}(i, c)
	}
	wg.Wait()
	return out
}

func safeRun(ctx context.Context, t tool.Tool, env *tool.Env, args json.RawMessage) (res string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return t.Run(ctx, env, args)
}

const (
	keepRecentTools = 6
	elidedKeep      = 300
)

// size estimates the conversation size in characters.
func (a *Agent) size() int {
	n := len(a.System)
	for _, m := range a.Messages {
		n += len(m.Text)
		for _, c := range m.ToolCalls {
			n += len(c.Args) + len(c.Name)
		}
	}
	return n
}

// elide shortens old tool outputs once the soft budget is exceeded. It
// works in one batch so the cached prefix changes rarely.
func (a *Agent) elide() {
	if a.ContextChars <= 0 || a.size() <= a.ContextChars {
		return
	}
	seen := 0
	for i := len(a.Messages) - 1; i >= 0; i-- {
		m := &a.Messages[i]
		if m.Role != provider.RoleTool {
			continue
		}
		seen++
		if seen <= keepRecentTools || len(m.Text) <= elidedKeep+100 {
			continue
		}
		cut := len(m.Text) - elidedKeep
		m.Text = strings.ToValidUTF8(m.Text[:elidedKeep], "") + fmt.Sprintf("\n[elided %d chars of old output; rerun the tool if needed]", cut)
	}
}

// Reset clears the conversation.
func (a *Agent) Reset() {
	a.Messages = nil
}
