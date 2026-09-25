// Package agent runs the model/tool loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
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
	// SubToolStart observes tool calls made by sub-agents.
	SubToolStart func(call provider.ToolCall)
	// SubAgentTool also names the sub-agent: its task prompt.
	SubAgentTool func(task string, c provider.ToolCall)
	// ToolOutput receives a running command's output as it arrives.
	ToolOutput func(call provider.ToolCall, chunk []byte)
}

// Agent holds one conversation.
type Agent struct {
	// ctxUsed is the prompt size of the latest model call, in tokens.
	ctxUsed atomic.Int64
	// MaxOutput is the model's output token limit (0 if unknown).
	MaxOutput int
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
	// Sub, if set, is the model sub-agents use (config "subagent_model"),
	// with its pricing and context size.
	Sub *SubModel
	// Oracle, if set, is a stronger model to consult (the oracle tool).
	Oracle *Oracle
	// Agents are the user's specialist sub-agents (the task tool's agent).
	Agents []AgentDef
	// PreTool, if set, runs before each tool call (config hooks.pre_tool);
	// an error blocks the call and is what the model is told.
	PreTool func(ctx context.Context, c provider.ToolCall) error
	// Notes, if set, returns what the user said when approving a step
	// ("yes, and …"); whichever agent (lead or sub-agent) runs its next
	// step gets it, since that is the one that asked.
	Notes func() []string
	// Typed, if set, is what the user typed for the next Run (the input
	// may carry context around it); it is kept with the message.
	Typed string
	// Steer, if set, returns messages the user sent while the agent was
	// working; they are given to the model after the current step.
	Steer func() []string
	// OnStep runs after each tool round, with the history well formed
	// (every call answered): the session saves it, so a crash mid-turn
	// keeps the steps already done.
	OnStep func()
	// Attach holds images for the next Run's user message.
	Attach []provider.Image
	// Ledger is the harness-kept working state (files, commands, errors,
	// todos) that survives compaction.
	Ledger Ledger

	depth int        // 0 for the main agent, 1 for sub-agents
	mu    sync.Mutex // guards Usage/Spent updates from parallel sub-agents
}

// SubModel is a model for sub-agents.
type SubModel struct {
	Client        provider.Client
	Model         string
	Cost          func(provider.Usage) float64
	ContextTokens int
	MaxOutput     int
	Reasoning     provider.Reasoning
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
	checkFailed bool // the latest build/test/lint run failed
	reminded    int  // verification reminders given this turn
	todoNudged  bool
	task        string
	fileEdits   map[string]int
	editWarned  map[string]bool
	truncations int
	sigs        []string
	failStreak  int // consecutive tool batches with a failure
	escalations int
	// overflowRetried: the provider said the prompt was too long once
	// this turn and the history was shrunk.
	overflowRetried bool
}

// Run sends input and loops until the model stops calling tools.
func (a *Agent) Run(ctx context.Context, input string) (Stats, error) {
	start := time.Now()
	var st Stats
	rs := runState{task: input, fileEdits: map[string]int{}, editWarned: map[string]bool{}}
	typed := firstNonEmptyStr(a.Typed, input)
	a.Typed = ""
	if a.Note != "" {
		input = "[" + a.Note + "]\n\n" + input
		a.Note = ""
	}
	a.Ledger.startTurn()
	// Thinking harder is for the moments that need it; each user turn
	// starts again at the configured level.
	base := a.Reasoning.Effort
	defer func() { a.Reasoning.Effort = base }()
	if a.Env != nil {
		a.Env.StartTurn()
		if a.Env.Gate != nil && a.Env.Gate.GetMode() == policy.Plan {
			input += "\n\n" + policy.PlanNote
		}
	}
	a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser, Text: input, Images: a.Attach, Typed: typed, At: start})
	a.Attach = nil
	// MaxTurns: 0 means the default (100, for one-shot runs and scripts);
	// below 0, no step limit (an interactive session works until the task
	// is done; the stuck detector and the cost limit still stop a loop).
	maxTurns := a.MaxTurns
	if maxTurns == 0 {
		maxTurns = 100
	}
	if maxTurns < 0 {
		maxTurns = math.MaxInt
	}
	defs := tool.Defs(a.Tools)
	before := a.Usage
	done := func(err error) (Stats, error) {
		st.Elapsed = time.Since(start)
		// Include sub-agents' usage, which is added to a.Usage directly.
		st.Usage = a.Usage.Minus(before)
		return st, err
	}
	for st.Turns < maxTurns {
		// Routine elision happens only at the start of a user turn, when no
		// tool round is in flight (its thinking block must stay intact);
		// mid-round only the emergency compaction threshold applies.
		a.manageContext(ctx, st.Turns == 0)
		resp, err := a.call(ctx, provider.Request{
			Model: a.Model, System: a.System, Messages: a.Messages, Tools: defs, MaxTokens: a.MaxTokens,
			MaxOutput: a.MaxOutput, Reasoning: a.Reasoning, Fast: a.FastMode,
		})
		st.Turns++
		a.Turns++
		st.Usage.Add(resp.Usage)
		a.Charge(resp.Usage, true) // under the lock sub-agents charge with
		if in := resp.Usage.Input + resp.Usage.CacheRead + resp.Usage.CacheWrite; in > 0 {
			a.ctxUsed.Store(int64(in))
		}
		if err != nil && contextOverflow(err) && !rs.overflowRetried && ctx.Err() == nil {
			// The estimate was off (thinking blocks, tool schemas): shrink
			// the history hard and try once more instead of failing every
			// turn from now on.
			rs.overflowRetried = true
			a.notice("the conversation no longer fits the model's context; compacting")
			if cerr := a.compact(ctx); cerr != nil {
				a.elide(1)
			}
			continue
		}
		if err != nil {
			// Keep whatever text streamed so the conversation stays coherent
			// (whitespace alone would be rejected on the next request).
			if strings.TrimSpace(resp.Text) != "" {
				a.Messages = append(a.Messages, provider.Message{Role: provider.RoleAssistant, Text: resp.Text})
			}
			return done(err)
		}
		truncated := resp.StopReason == "max_tokens" || resp.StopReason == "length"
		if truncated {
			resp.ToolCalls = validCalls(resp.ToolCalls)
			// The call being generated when the output limit hit is cut off
			// even if its arguments happen to parse ("" becomes "{}").
			if n := len(resp.ToolCalls); n > 0 && lastBlockIsToolUse(resp.Raw) {
				resp.ToolCalls = resp.ToolCalls[:n-1]
			}
		}
		resp.ToolCalls = sanitizeCalls(resp.ToolCalls)
		served := a.Model
		if resp.Model != "" {
			served = resp.Model
		}
		msg := provider.Message{Role: provider.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls,
			Raw: resp.Raw, RawModel: served, Reasoning: resp.Reasoning, ReasoningField: resp.ReasoningField}
		if truncated {
			// Keep thinking and text (the API requires the thinking block
			// before this turn's tool results), drop cut-off tool calls.
			msg.Raw = keepRawBlocks(resp.Raw, resp.ToolCalls)
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
			if a.OnStep != nil {
				a.OnStep()
			}
			if ctx.Err() != nil {
				return done(ctx.Err())
			}
			if stuck {
				a.notice("stopped: repeating the same action")
				return done(ErrStuck)
			}
			if a.Notes != nil {
				// Said while approving one of this agent's steps.
				if notes := a.Notes(); len(notes) > 0 {
					a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser,
						Text: "[The user approved your last action and added:]\n" + strings.Join(notes, "\n\n")})
				}
			}
			if a.Steer != nil && a.depth == 0 {
				if msgs := a.Steer(); len(msgs) > 0 {
					a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser,
						Text: "[The user sent this while you were working; take it into account now:]\n" + strings.Join(msgs, "\n\n")})
				}
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
		if a.Verify && (rs.editedCode || rs.checkFailed) && rs.reminded < 2 {
			// The verification gate: an unverified or failing change is not
			// done. The task is quoted back, since it may be far above.
			rs.reminded++
			why := "You changed code but have not run a build, test or lint since."
			if !rs.editedCode {
				why = "Your latest build/test/lint run failed."
			}
			a.notice("asking the model to verify its change")
			a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser,
				Text: "[agentium] " + why + " Before finishing: re-read the task — «" + clipTask(rs.task) + "» — run the most relevant check, and make sure every requirement is met (do not weaken or delete tests to pass). If something cannot be verified or fixed, say so in one line."})
			continue
		}
		if open := openTodos(a.Ledger.Todos()); len(open) > 0 && !rs.todoNudged && a.depth == 0 {
			// Finishing with open plan items usually means a forgotten step.
			rs.todoNudged = true
			a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser,
				Text: "[agentium] Your todo list still has open items: " + strings.Join(open, "; ") + ". Finish them, or update the list (mark done, or drop what no longer applies) before you reply."})
			continue
		}
		return done(nil)
	}
	return done(ErrMaxTurns)
}

// ContextUsed reports how much of the context window the conversation
// took at the latest model call; safe to call while the agent runs.
func (a *Agent) ContextUsed() (used, limit int) {
	return int(a.ctxUsed.Load()), a.ContextTokens
}

// openTodos lists plan items not marked done.
func openTodos(ts []Todo) []string {
	var out []string
	for _, t := range ts {
		if t.Status != "done" {
			out = append(out, t.Text)
		}
	}
	return out
}

// clipTask shortens the task for a reminder.
func clipTask(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = strings.ToValidUTF8(s[:600], "") + "…"
	}
	return s
}

func (a *Agent) notice(msg string) {
	if a.Events.Notice != nil {
		a.Events.Notice(msg)
	}
}

// validCalls drops tool calls whose arguments were cut off mid-JSON.
// contextOverflow reports a provider error saying the prompt is too long.
func contextOverflow(err error) bool {
	m := strings.ToLower(err.Error())
	for _, s := range []string{"prompt is too long", "context_length_exceeded", "maximum context length",
		"context length", "too many tokens", "input is too long", "exceeds the context window", "request too large"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// lastBlockIsToolUse reports whether an Anthropic-style raw content list
// ends with a tool_use block (without raw blocks: assume it does).
func lastBlockIsToolUse(raw json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return true
	}
	return blocks[len(blocks)-1].Type == "tool_use"
}

// keepRawBlocks filters raw content blocks to thinking, text and the
// tool_use blocks of calls that are kept.
func keepRawBlocks(raw json.RawMessage, keep []provider.ToolCall) json.RawMessage {
	var blocks []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	ids := map[string]bool{}
	for _, c := range keep {
		ids[c.ID] = true
	}
	var out []json.RawMessage
	for _, b := range blocks {
		var h struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(b, &h) != nil {
			continue
		}
		switch h.Type {
		case "thinking", "redacted_thinking", "text":
			out = append(out, b)
		case "tool_use":
			if ids[h.ID] {
				out = append(out, b)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	r, _ := json.Marshal(out)
	return r
}

// sanitizeCalls keeps a model's malformed tool calls from poisoning the
// conversation: every call gets a name and an id unique within the reply,
// and arguments that are not JSON are set aside (the history, saved and
// replayed on every request, must stay valid).
func sanitizeCalls(calls []provider.ToolCall) []provider.ToolCall {
	seen := map[string]bool{}
	for i := range calls {
		c := &calls[i]
		if c.Name == "" {
			c.Name = "unnamed_tool"
		}
		if c.ID == "" || seen[c.ID] {
			c.ID = fmt.Sprintf("call_fix_%d_%d", time.Now().UnixNano(), i)
		}
		seen[c.ID] = true
		if len(c.Args) == 0 {
			c.Args = json.RawMessage("{}")
		} else if !json.Valid(c.Args) {
			c.BadArgs = string(c.Args)
			c.Args = json.RawMessage("{}")
		}
	}
	return calls
}

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
	if a.Env != nil && !a.Env.Vision {
		req.Messages = withoutImages(req.Messages)
	}
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
			var imgs []provider.Image
			t, ok := byName[c.Name]
			var blocked error
			if ok && a.PreTool != nil && c.BadArgs == "" && (len(c.Args) == 0 || json.Valid(c.Args)) {
				blocked = a.PreTool(ctx, c)
			}
			switch {
			case !ok:
				err = fmt.Errorf("unknown tool %q", c.Name)
			case c.BadArgs != "":
				err = fmt.Errorf("the arguments were not valid JSON (cut off?): %.120s — send the call again with complete JSON", c.BadArgs)
			case len(c.Args) > 0 && !json.Valid(c.Args):
				err = errors.New("arguments are not valid JSON")
			case blocked != nil:
				err = fmt.Errorf("blocked by the user's pre_tool hook: %w", blocked)
			default:
				tctx, images := tool.WithImageSink(context.WithValue(ctx, agentKey{}, a))
				if f := a.Events.ToolOutput; f != nil {
					lw := newLiveWriter(func(p []byte) { f(c, p) })
					defer lw.close()
					tctx = tool.WithLive(tctx, lw)
				}
				res, err = safeRun(tctx, t, a.Env, c.Args)
				imgs = images()
				a.Ledger.record(c.Name, c.Args, res, err)
			}
			took := time.Since(t0)
			if a.Events.ToolDone != nil {
				a.Events.ToolDone(c, res, err, took)
			}
			if took > 20*time.Second && c.Name == "bash" {
				// Knowing a command is slow keeps the model from re-running
				// it for another look at its output.
				res += fmt.Sprintf("\n[took %s]", took.Round(time.Second))
			}
			msg := provider.Message{Role: provider.RoleTool, ToolCallID: c.ID, Text: res, Images: imgs}
			if errors.Is(err, context.Canceled) {
				err = errors.New("interrupted by the user before it finished")
			}
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

// liveWriter passes a command's output to the UI without ever blocking
// the command: chunks go through a buffer, and when the UI falls behind
// (a paused terminal) they are dropped; the tool result is unaffected.
type liveWriter struct {
	mu     sync.Mutex
	closed bool
	ch     chan []byte
}

func newLiveWriter(f func([]byte)) *liveWriter {
	w := &liveWriter{ch: make(chan []byte, 64)}
	go func() {
		for p := range w.ch {
			f(p)
		}
	}()
	return w
}

func (w *liveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		select {
		case w.ch <- append([]byte(nil), p...):
		default:
		}
	}
	return len(p), nil
}

// close stops delivery; late writes from leftover children are dropped.
func (w *liveWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
}

func safeRun(ctx context.Context, t tool.Tool, env *tool.Env, args json.RawMessage) (res string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return t.Run(ctx, env, args)
}

// Charge adds usage from a call made outside Run (a side question, a
// suggestion); priced only when priced is set (the call used this
// agent's own model, whose price Cost knows).
func (a *Agent) Charge(us provider.Usage, priced bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Usage.Add(us)
	if priced && a.Cost != nil {
		a.Spent += a.Cost(us)
	}
}

// Reset clears the conversation.
func (a *Agent) Reset() {
	a.Messages = nil
	a.ctxUsed.Store(0)
	if a.Env != nil {
		a.Env.ForgetReads()
	}
}

// withoutImages replaces images with a note for models that cannot view
// them (e.g. after /model switched away from a vision model), without
// touching the stored history.
func withoutImages(ms []provider.Message) []provider.Message {
	var out []provider.Message
	for i, m := range ms {
		if len(m.Images) == 0 {
			continue
		}
		if out == nil {
			out = append([]provider.Message(nil), ms...)
		}
		out[i].Images = nil
		out[i].Text += fmt.Sprintf("\n[%d image(s) omitted: this model cannot view images]", len(m.Images))
	}
	if out == nil {
		return ms
	}
	return out
}

type agentKey struct{}

// running returns the agent whose tool call this is (a sub-agent shares
// its parent's tool list, so tools bound to an agent look it up here).
func running(ctx context.Context, fallback *Agent) *Agent {
	if ag, ok := ctx.Value(agentKey{}).(*Agent); ok {
		return ag
	}
	return fallback
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Totals is the session's usage and cost so far (sub-agents included).
func (a *Agent) Totals() (provider.Usage, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Usage, a.Spent
}
