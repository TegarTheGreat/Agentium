// Package agent runs the model/tool loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/policy"
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
	// Notice reports harness actions the user may want to know about
	// (verification reminder, compaction, stuck detection, truncation).
	Notice func(msg string)
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
	// ContextTokens is the model's context window. It sets when old tool
	// output is elided and when the conversation is compacted. Zero
	// disables both (and ContextChars, if set, is used directly).
	ContextTokens int
	// ContextChars is the soft budget in characters before elision.
	// Derived from ContextTokens when zero.
	ContextChars int
	// Fast, if set, does background work (compaction summaries) cheaply.
	Fast      provider.Client
	FastModel string
	// Verify makes the agent remind the model, once per run, to run a
	// check when it tries to finish after changing code without one.
	Verify bool
	// Reasoning configures thinking (effort and model capabilities).
	Reasoning provider.Reasoning
	// FastMode requests the provider's fast output mode.
	FastMode bool
	// Cost prices a model call's usage in USD (nil = unknown); with
	// MaxCost > 0 the run stops once the session total passes it.
	Cost    func(provider.Usage) float64
	MaxCost float64
	Spent   float64
	// OnRemember receives durable facts surfaced during compaction.
	OnRemember func(fact string)
	Events     Events

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

var (
	// ErrMaxTurns is returned when the loop hits MaxTurns.
	ErrMaxTurns = errors.New("stopped: reached max turns")
	// ErrStuck is returned when the model keeps repeating the same action
	// with the same result.
	ErrStuck = errors.New("stopped: the same action kept giving the same result")
	// ErrRefused is returned when the provider's safety system declined.
	ErrRefused = errors.New("stopped: the model declined the request")
	// ErrBudget is returned when MaxCost is reached.
	ErrBudget = errors.New("stopped: cost limit reached")
	// ErrTruncated is returned when replies keep hitting the output limit.
	ErrTruncated = errors.New("stopped: replies keep hitting the output token limit")
)

const (
	stuckWarn      = 3
	stuckStop      = 5
	stuckWindow    = 16
	maxTruncations = 2
)

// runState is per-Run bookkeeping.
type runState struct {
	editedCode  bool
	reminded    bool
	truncations int
	sigs        []string
}

// Run sends input and loops until the model stops calling tools.
func (a *Agent) Run(ctx context.Context, input string) (Stats, error) {
	start := time.Now()
	var st Stats
	var rs runState
	if a.Note != "" {
		input = "[" + a.Note + "]\n\n" + input
		a.Note = ""
	}
	if a.Env != nil {
		a.Env.StartTurn()
		if a.Env.Gate != nil && a.Env.Gate.GetMode() == policy.Plan {
			input += "\n\n" + policy.PlanNote
		}
	}
	a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser, Text: input})
	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 100
	}
	defs := tool.Defs(a.Tools)
	done := func(err error) (Stats, error) {
		st.Elapsed = time.Since(start)
		return st, err
	}
	for st.Turns < maxTurns {
		// Routine elision happens only at the start of a user turn, when no
		// tool round is in flight (its thinking block must stay intact);
		// mid-round only the emergency compaction threshold applies.
		a.manageContext(ctx, st.Turns == 0)
		resp, err := a.call(ctx, provider.Request{
			Model: a.Model, System: a.System, Messages: a.Messages, Tools: defs, MaxTokens: a.MaxTokens,
			Reasoning: a.Reasoning, Fast: a.FastMode,
		})
		st.Turns++
		a.Turns++
		st.Usage.Add(resp.Usage)
		a.Usage.Add(resp.Usage)
		if a.Cost != nil {
			a.Spent += a.Cost(resp.Usage)
		}
		if err != nil {
			// Keep whatever text streamed so the conversation stays coherent.
			if resp.Text != "" {
				a.Messages = append(a.Messages, provider.Message{Role: provider.RoleAssistant, Text: resp.Text})
			}
			return done(err)
		}
		truncated := resp.StopReason == "max_tokens" || resp.StopReason == "length"
		if truncated {
			resp.ToolCalls = validCalls(resp.ToolCalls)
		}
		served := a.Model
		if resp.Model != "" {
			served = resp.Model
		}
		msg := provider.Message{Role: provider.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls,
			Raw: resp.Raw, RawModel: served, Reasoning: resp.Reasoning}
		if truncated {
			msg.Raw = nil // it may hold a cut-off tool call we dropped
		}
		a.Messages = append(a.Messages, msg)
		if resp.StopReason == "refusal" {
			a.notice("the model declined this request")
			return done(ErrRefused)
		}
		if a.Events.TurnFinish != nil {
			a.Events.TurnFinish(resp)
		}
		if a.MaxCost > 0 && a.Spent >= a.MaxCost && (len(resp.ToolCalls) > 0 || truncated) {
			a.notice(fmt.Sprintf("cost limit $%.2f reached ($%.4f spent)", a.MaxCost, a.Spent))
			// Leave the history well-formed: answer the pending calls.
			for _, c := range resp.ToolCalls {
				a.Messages = append(a.Messages, provider.Message{Role: provider.RoleTool, ToolCallID: c.ID, IsError: true, Text: "not run: cost limit reached"})
			}
			return done(ErrBudget)
		}
		if len(resp.ToolCalls) > 0 {
			st.ToolCalls += len(resp.ToolCalls)
			results := a.runTools(ctx, resp.ToolCalls)
			stuck := a.track(&rs, resp.ToolCalls, results)
			a.Messages = append(a.Messages, results...)
			if ctx.Err() != nil {
				return done(ctx.Err())
			}
			if stuck {
				a.notice("stopped: repeating the same action")
				return done(ErrStuck)
			}
		}
		if truncated {
			rs.truncations++
			if rs.truncations > maxTruncations {
				return done(ErrTruncated)
			}
			a.notice("reply hit the output limit; asking the model to continue")
			a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser,
				Text: "[agentium] Your last reply was cut off at the output token limit. Continue from where you stopped; split large file writes into several smaller edits."})
			continue
		}
		if len(resp.ToolCalls) > 0 {
			continue
		}
		if a.Verify && rs.editedCode && !rs.reminded {
			rs.reminded = true
			a.notice("code changed without a check; asking the model to verify")
			a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser,
				Text: "[agentium] You changed code but have not run a build, test or lint since. Run the most relevant quick check now. If it cannot be verified, reply with one line saying why."})
			continue
		}
		return done(nil)
	}
	return done(ErrMaxTurns)
}

func (a *Agent) notice(msg string) {
	if a.Events.Notice != nil {
		a.Events.Notice(msg)
	}
}

// validCalls drops tool calls whose arguments were cut off mid-JSON.
func validCalls(calls []provider.ToolCall) []provider.ToolCall {
	var out []provider.ToolCall
	for _, c := range calls {
		if c.Name != "" && json.Valid(c.Args) {
			out = append(out, c)
		}
	}
	return out
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

// Reset clears the conversation.
func (a *Agent) Reset() {
	a.Messages = nil
}
