package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/tool"
)

// script is a fake model that replays responses and records requests.
type script struct {
	mu    sync.Mutex
	steps []func(req provider.Request) (provider.Response, error)
	reqs  []provider.Request
}

func (s *script) Stream(_ context.Context, req provider.Request, onText func(string)) (provider.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := req
	cp.Messages = append([]provider.Message(nil), req.Messages...)
	s.reqs = append(s.reqs, cp)
	if len(s.steps) == 0 {
		return provider.Response{Text: "done"}, nil
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	r, err := step(req)
	if r.Text != "" && onText != nil {
		onText(r.Text)
	}
	return r, err
}

func calls(cs ...provider.ToolCall) func(provider.Request) (provider.Response, error) {
	return func(provider.Request) (provider.Response, error) {
		return provider.Response{ToolCalls: cs, Usage: provider.Usage{Input: 10, Output: 5}}, nil
	}
}

func tc(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}
}

func newAgent(t *testing.T, s *script) *Agent {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	return &Agent{
		Client: s, Model: "fake", System: SystemPrompt(dir, false, ""), Tools: tool.All(),
		Env: &tool.Env{Root: dir, Gate: &policy.Gate{Mode: policy.Auto, Root: dir}},
	}
}

func TestLoopRunsToolsAndStops(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "edit", `{"path":"a.txt","new":"alpha"}`), tc("2", "edit", `{"path":"b.txt","new":"beta"}`)),
		calls(tc("3", "read", `{"path":"a.txt"}`), tc("4", "nope", `{}`), tc("5", "read", `{bad json`)),
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "All set."}, nil },
	}}
	a := newAgent(t, s)
	var text strings.Builder
	a.Events.Text = func(d string) { text.WriteString(d) }
	st, err := a.Run(context.Background(), "make files")
	if err != nil {
		t.Fatal(err)
	}
	if st.Turns != 3 || st.ToolCalls != 5 || st.Usage.Input != 20 {
		t.Fatalf("stats = %+v", st)
	}
	if text.String() != "All set." {
		t.Fatalf("text = %q", text.String())
	}
	b, _ := os.ReadFile(filepath.Join(a.Env.Root, "b.txt"))
	if string(b) != "beta" {
		t.Fatalf("b.txt = %q", b)
	}
	// Results come back in call order with errors flagged.
	last := s.reqs[2].Messages
	res := last[len(last)-3:]
	if res[0].ToolCallID != "3" || res[0].Text != "1\talpha" || res[0].IsError {
		t.Fatalf("read result = %+v", res[0])
	}
	if !res[1].IsError || !strings.Contains(res[1].Text, "unknown tool") {
		t.Fatalf("unknown tool result = %+v", res[1])
	}
	if !res[2].IsError || !strings.Contains(res[2].Text, "not valid JSON") {
		t.Fatalf("bad args result = %+v", res[2])
	}
	// System prompt and tools are identical on every turn (cacheable).
	for _, r := range s.reqs[1:] {
		if r.System != s.reqs[0].System || len(r.Tools) != 5 {
			t.Fatal("prefix changed between turns")
		}
	}
}

func TestToolsRunInParallel(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "bash", `{"cmd":"sleep 0.4"}`), tc("2", "bash", `{"cmd":"sleep 0.4"}`), tc("3", "bash", `{"cmd":"sleep 0.4"}`)),
	}}
	a := newAgent(t, s)
	t0 := time.Now()
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 1*time.Second {
		t.Fatalf("3×0.4s tools took %s; not parallel", d)
	}
}

func TestRetryOnlyBeforeStreaming(t *testing.T) {
	n := 0
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		func(provider.Request) (provider.Response, error) {
			n++
			return provider.Response{}, &provider.HTTPError{Status: 429}
		},
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "ok"}, nil },
	}}
	a := newAgent(t, s)
	var retries int
	a.Events.Retry = func(error, time.Duration) { retries++ }
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if retries != 1 {
		t.Fatalf("retries = %d", retries)
	}

	// A connection drop after text started streaming is retried too.
	s3 := &script{steps: []func(provider.Request) (provider.Response, error){
		func(provider.Request) (provider.Response, error) {
			return provider.Response{Text: "partial"}, provider.ErrIncomplete
		},
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "full answer"}, nil },
	}}
	a3 := newAgent(t, s3)
	a3.Events.Retry = func(error, time.Duration) {}
	if _, err := a3.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if last := a3.Messages[len(a3.Messages)-1]; last.Text != "full answer" {
		t.Fatalf("kept %q", last.Text)
	}

	s2 := &script{steps: []func(provider.Request) (provider.Response, error){
		func(provider.Request) (provider.Response, error) {
			return provider.Response{}, &provider.HTTPError{Status: 401, Body: "bad key"}
		},
	}}
	if _, err := newAgent(t, s2).Run(context.Background(), "hi"); err == nil {
		t.Fatal("non-retryable error must surface")
	}
}

func TestMaxTurns(t *testing.T) {
	loop := func(provider.Request) (provider.Response, error) {
		return provider.Response{ToolCalls: []provider.ToolCall{tc("x", "read", `{"path":"."}`)}}, nil
	}
	s := &script{steps: []func(provider.Request) (provider.Response, error){loop, loop, loop, loop}}
	a := newAgent(t, s)
	a.MaxTurns = 3
	if _, err := a.Run(context.Background(), "spin"); !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("err = %v", err)
	}
}

func TestElide(t *testing.T) {
	a := &Agent{ContextChars: 5000}
	big := strings.Repeat("z", 2000)
	for i := 0; i < 10; i++ {
		a.Messages = append(a.Messages,
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{tc("i", "read", `{}`)}},
			provider.Message{Role: provider.RoleTool, ToolCallID: "i", Text: big})
	}
	a.elide(keepRecentTools)
	elided := 0
	for i, m := range a.Messages {
		if m.Role != provider.RoleTool {
			continue
		}
		if strings.Contains(m.Text, "[elided") {
			elided++
			if i >= len(a.Messages)-2*keepRecentTools {
				t.Fatal("recent tool output was elided")
			}
		}
	}
	if elided != 10-keepRecentTools {
		t.Fatalf("elided %d, want %d", elided, 10-keepRecentTools)
	}
	// Under budget: untouched.
	b := &Agent{ContextChars: 1 << 30, Messages: []provider.Message{{Role: provider.RoleTool, Text: big}}}
	b.manageContext(context.Background(), true)
	if b.Messages[0].Text != big {
		t.Fatal("elided under budget")
	}
}

func TestCancelStopsTools(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "bash", `{"cmd":"sleep 10"}`)),
	}}
	a := newAgent(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	t0 := time.Now()
	_, err := a.Run(ctx, "go")
	if !errors.Is(err, context.Canceled) || time.Since(t0) > 3*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(t0))
	}
	// Conversation stays well-formed: the tool call has a result.
	last := a.Messages[len(a.Messages)-1]
	if last.Role != provider.RoleTool || last.ToolCallID != "1" {
		t.Fatalf("last message = %+v", last)
	}
}

func TestSystemPromptSmallAndIncludesAgentsMD(t *testing.T) {
	t.Setenv("AGENTIUM_HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	sub := filepath.Join(dir, "pkg")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use tabs."), 0o644)
	os.WriteFile(filepath.Join(sub, "CLAUDE.md"), []byte("Pkg rule."), 0o644)
	p := SystemPrompt(sub, false, "")
	if !strings.Contains(p, "Use tabs.") || !strings.Contains(p, "Pkg rule.") || !strings.Contains(p, "git=yes") {
		t.Fatalf("prompt missing context:\n%s", p)
	}
	if strings.Index(p, "Use tabs.") > strings.Index(p, "Pkg rule.") {
		t.Fatal("root instructions should come before nested ones")
	}
	if len(basePrompt) > 1500 {
		t.Fatalf("base prompt grew to %d chars; keep it lean", len(basePrompt))
	}
}

func TestVerifyReminder(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "edit", `{"path":"main.go","new":"package main\n"}`)),
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "Done."}, nil },
		calls(tc("2", "bash", `{"cmd":"echo test ok"}`)),
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "Verified."}, nil },
	}}
	a := newAgent(t, s)
	a.Verify = true
	var notes []string
	a.Events.Notice = func(m string) { notes = append(notes, m) }
	st, err := a.Run(context.Background(), "write main.go")
	if err != nil || st.Turns != 4 {
		t.Fatalf("turns=%d err=%v", st.Turns, err)
	}
	if len(notes) != 1 || !strings.Contains(s.reqs[2].Messages[len(s.reqs[2].Messages)-1].Text, "have not run a build") {
		t.Fatalf("reminder missing: %v", notes)
	}
	// Non-code edits (docs) and edits followed by a check get no reminder.
	s2 := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "edit", `{"path":"README.md","new":"hi"}`)),
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "Done."}, nil },
	}}
	a2 := newAgent(t, s2)
	a2.Verify = true
	if st, _ := a2.Run(context.Background(), "docs"); st.Turns != 2 {
		t.Fatalf("docs edit should not trigger verify, turns=%d", st.Turns)
	}
}

func TestStuckDetector(t *testing.T) {
	same := calls(tc("x", "bash", `{"cmd":"echo same"}`))
	var steps []func(provider.Request) (provider.Response, error)
	for i := 0; i < 10; i++ {
		steps = append(steps, same)
	}
	s := &script{steps: steps}
	a := newAgent(t, s)
	st, err := a.Run(context.Background(), "loop")
	if !errors.Is(err, ErrStuck) || st.Turns != stuckStop {
		t.Fatalf("err=%v turns=%d", err, st.Turns)
	}
	warned := false
	for _, m := range a.Messages {
		if m.Role == provider.RoleTool && strings.Contains(m.Text, "change your approach") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("model should be warned before being stopped")
	}
	// Same command with different results (e.g. polling) is not stuck.
	n := 0
	var poll []func(provider.Request) (provider.Response, error)
	for i := 0; i < 6; i++ {
		poll = append(poll, calls(tc("p", "bash", `{"cmd":"date +%N"}`)))
	}
	s2 := &script{steps: poll}
	a2 := newAgent(t, s2)
	_, err = a2.Run(context.Background(), "poll")
	_ = n
	if errors.Is(err, ErrStuck) {
		t.Fatal("changing results must not count as stuck")
	}
}

func TestTruncatedReply(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		func(provider.Request) (provider.Response, error) {
			return provider.Response{Text: "Writing", StopReason: "max_tokens",
				ToolCalls: []provider.ToolCall{tc("1", "edit", `{"path":"a.txt","new":"unterminated`)}}, nil
		},
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "ok"}, nil },
	}}
	a := newAgent(t, s)
	st, err := a.Run(context.Background(), "big file")
	if err != nil || st.Turns != 2 {
		t.Fatalf("turns=%d err=%v", st.Turns, err)
	}
	// The cut-off call was dropped and the model was told to continue.
	if len(a.Messages[1].ToolCalls) != 0 {
		t.Fatal("invalid truncated tool call should be dropped")
	}
	if !strings.Contains(a.Messages[2].Text, "cut off") {
		t.Fatalf("continue note missing: %+v", a.Messages[2])
	}
	// Endless truncation gives up.
	var steps []func(provider.Request) (provider.Response, error)
	for i := 0; i < 5; i++ {
		steps = append(steps, func(provider.Request) (provider.Response, error) {
			return provider.Response{Text: "x", StopReason: "length"}, nil
		})
	}
	if _, err := newAgent(t, &script{steps: steps}).Run(context.Background(), "x"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
}

func TestCompaction(t *testing.T) {
	fast := &script{steps: []func(provider.Request) (provider.Response, error){
		func(provider.Request) (provider.Response, error) {
			return provider.Response{Text: "Goal: build X.\n@remember project uses tabs\nState: half done."}, nil
		},
	}}
	s := &script{}
	a := newAgent(t, s)
	a.ContextTokens = 20000 // tiny window: forces compaction
	a.MaxTokens = 1000
	a.Fast, a.FastModel = fast, "fast"
	var remembered []string
	a.OnRemember = func(f string) { remembered = append(remembered, f) }
	big := strings.Repeat("y", 9000)
	for i := 0; i < 8; i++ {
		a.Messages = append(a.Messages,
			provider.Message{Role: provider.RoleUser, Text: fmt.Sprintf("step %d %s", i, big)},
			provider.Message{Role: provider.RoleAssistant, Text: "ok " + big})
	}
	if _, err := a.Run(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	if len(fast.reqs) != 1 {
		t.Fatalf("fast model should summarize once, got %d", len(fast.reqs))
	}
	first := a.Messages[0].Text
	if !strings.HasPrefix(first, "[Summary of the earlier conversation]") || strings.Contains(first, "@remember") {
		t.Fatalf("summary message = %q", first)
	}
	if len(remembered) != 1 || remembered[0] != "project uses tabs" {
		t.Fatalf("remembered = %v", remembered)
	}
	if a.size() > 60000 {
		t.Fatalf("still too big after compaction: %d", a.size())
	}
	// The kept tail starts at a user message and still ends with the new turn.
	if a.Messages[1].Role != provider.RoleUser || a.Messages[len(a.Messages)-2].Text != "continue" {
		t.Fatal("tail not preserved")
	}
}

func TestElideArgs(t *testing.T) {
	long := strings.Repeat("z", 5000)
	raw, _ := json.Marshal(map[string]any{"path": "f.go", "new": long})
	out := elideArgs(raw)
	var m map[string]any
	if json.Unmarshal(out, &m) != nil || m["path"] != "f.go" || !strings.Contains(m["new"].(string), "elided") {
		t.Fatalf("elideArgs = %s", out)
	}
	short := json.RawMessage(`{"path":"x"}`)
	if string(elideArgs(short)) != string(short) {
		t.Fatal("short args untouched")
	}
}

func TestRawInvalidatedAfterEdits(t *testing.T) {
	build := func(model string) *Agent {
		a := &Agent{ContextChars: 3000}
		big := strings.Repeat("q", 2000)
		for i := 0; i < 9; i++ {
			a.Messages = append(a.Messages,
				provider.Message{Role: provider.RoleAssistant, Raw: json.RawMessage(`[{"type":"thinking","signature":"S"}]`), RawModel: model,
					ToolCalls: []provider.ToolCall{tc(fmt.Sprint(i), "read", `{}`)}},
				provider.Message{Role: provider.RoleTool, ToolCallID: fmt.Sprint(i), Text: big})
		}
		a.elide(keepRecentTools)
		return a
	}
	// Prefix-bound models: blocks after the first edit go, except the
	// in-flight assistant turn (the last one, whose results just arrived).
	a := build("claude-opus-5-5")
	firstEdited := -1
	for i, m := range a.Messages {
		if strings.Contains(m.Text, "[elided") {
			firstEdited = i
			break
		}
	}
	last := len(a.Messages) - 2
	for i, m := range a.Messages {
		if m.Role != provider.RoleAssistant {
			continue
		}
		switch {
		case i < firstEdited && m.Raw == nil:
			t.Fatalf("message %d before the edit lost its blocks", i)
		case i > firstEdited && i != last && m.Raw != nil:
			t.Fatalf("message %d after the edit kept stale blocks", i)
		case i == last && m.Raw == nil:
			t.Fatal("in-flight turn must keep its thinking block")
		}
	}
	// Older models' signatures don't depend on history: nothing is dropped.
	for _, m := range build("claude-sonnet-5").Messages {
		if m.Role == provider.RoleAssistant && m.Raw == nil {
			t.Fatal("non-prefix-bound blocks must be kept")
		}
	}
}

func TestCompactionInsideOneRun(t *testing.T) {
	fast := &script{steps: []func(provider.Request) (provider.Response, error){
		func(provider.Request) (provider.Response, error) {
			return provider.Response{Text: "state: halfway"}, nil
		},
	}}
	a := newAgent(t, &script{})
	a.ContextTokens, a.MaxTokens = 20000, 1000
	a.Fast, a.FastModel = fast, "fast"
	big := strings.Repeat("r", 6000)
	a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser, Text: "one long task"})
	for i := 0; i < 8; i++ {
		a.Messages = append(a.Messages,
			provider.Message{Role: provider.RoleAssistant, Text: big, ToolCalls: []provider.ToolCall{tc(fmt.Sprint(i), "read", `{}`)}},
			provider.Message{Role: provider.RoleTool, ToolCallID: fmt.Sprint(i), Text: "ok"})
	}
	if err := a.compact(context.Background()); err != nil {
		t.Fatalf("single-run compaction: %v", err)
	}
	if a.Messages[1].Role != provider.RoleAssistant || len(a.Messages[1].ToolCalls) == 0 {
		t.Fatalf("tail should start at an assistant turn: %+v", a.Messages[1])
	}
	if a.Messages[2].Role != provider.RoleTool || a.Messages[2].ToolCallID != a.Messages[1].ToolCalls[0].ID {
		t.Fatal("tool call and result must stay paired")
	}
}

func TestPlanModeNote(t *testing.T) {
	s := &script{}
	a := newAgent(t, s)
	a.Env.Gate.SetMode(policy.Plan)
	if _, err := a.Run(context.Background(), "add a flag"); err != nil {
		t.Fatal(err)
	}
	if got := s.reqs[0].Messages[0].Text; !strings.HasSuffix(got, policy.PlanNote) {
		t.Fatalf("plan note missing: %q", got)
	}
	if strings.Contains(s.reqs[0].System, "plan mode") {
		t.Fatal("plan note must stay out of the cached system prompt")
	}
}

func TestImagesFlowAndElide(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){calls(tc("1", "read", `{"path":"p.png"}`))}}
	a := newAgent(t, s)
	a.Env.Vision = true
	var buf bytes.Buffer
	png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1)))
	os.WriteFile(filepath.Join(a.Env.Root, "p.png"), buf.Bytes(), 0o644)
	a.Attach = []provider.Image{{MediaType: "image/png", Data: buf.Bytes()}}
	if _, err := a.Run(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}
	if len(a.Attach) != 0 || len(a.Messages[0].Images) != 1 {
		t.Fatal("user attachment not moved into the message")
	}
	var toolMsg *provider.Message
	for i := range a.Messages {
		if a.Messages[i].Role == provider.RoleTool {
			toolMsg = &a.Messages[i]
		}
	}
	if toolMsg == nil || len(toolMsg.Images) != 1 {
		t.Fatalf("tool image missing: %+v", a.Messages)
	}
	before := a.size()
	a.elide(0)
	if len(toolMsg.Images) != 0 || !strings.Contains(toolMsg.Text, "image(s) elided") || a.size() >= before {
		t.Fatalf("image not elided: %q", toolMsg.Text)
	}
}

func TestLedgerTodoAndCompactionState(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "todo", `{"items":[{"text":"fix parser","status":"in_progress"},{"text":"add test","status":"pending"}]}`)),
		calls(tc("2", "bash", `{"cmd":"echo boom >&2; exit 3"}`), tc("3", "read", `{"path":"a.txt"}`)),
	}}
	a := newAgent(t, s)
	a.Tools = append(a.Tools, a.TodoTool())
	os.WriteFile(filepath.Join(a.Env.Root, "a.txt"), []byte("hi"), 0o644)
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	st := a.Ledger.Render()
	for _, want := range []string{"1. [~] fix parser", "2. [ ] add test", "Files read: a.txt", "echo boom >&2; exit 3 (exit 3)", "Latest unresolved error:\nbash echo boom", "boom"} {
		if !strings.Contains(st, want) {
			t.Errorf("ledger missing %q:\n%s", want, st)
		}
	}
	if errs := a.Ledger.TurnErrors(); len(errs) != 1 || !strings.Contains(errs[0], "boom") {
		t.Fatalf("turn errors: %v", errs)
	}
	// The same command passing clears the unresolved error.
	a.Ledger.record("bash", json.RawMessage(`{"cmd":"echo boom >&2; exit 3"}`), "ok", nil)
	if strings.Contains(a.Ledger.Render(), "unresolved") {
		t.Fatal("error should clear once the command passes")
	}
	// Fetch marks the turn untrusted; a new turn resets it.
	a.Ledger.record("fetch", json.RawMessage(`{"url":"https://x"}`), "page", nil)
	if !a.Ledger.Untrusted() {
		t.Fatal("fetch should mark the turn untrusted")
	}
	a.Ledger.startTurn()
	if a.Ledger.Untrusted() || len(a.Ledger.TurnErrors()) != 0 {
		t.Fatal("per-turn state not reset")
	}

	// Compaction carries the ledger verbatim.
	fast := &script{steps: []func(provider.Request) (provider.Response, error){
		func(provider.Request) (provider.Response, error) {
			return provider.Response{Text: "Goal: fix parser."}, nil
		},
	}}
	a.ContextTokens, a.MaxTokens = 20000, 1000
	a.Fast, a.FastModel = fast, "fast"
	big := strings.Repeat("y", 9000)
	for i := 0; i < 8; i++ {
		a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser, Text: big}, provider.Message{Role: provider.RoleAssistant, Text: big})
	}
	if _, err := a.Run(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	if first := a.Messages[0].Text; !strings.Contains(first, "Goal: fix parser.") || !strings.Contains(first, "<session-state") || !strings.Contains(first, "1. [~] fix parser") {
		t.Fatalf("compacted state: %q", first)
	}
}

func TestImagesStrippedForTextOnlyModel(t *testing.T) {
	s := &script{}
	a := newAgent(t, s)
	a.Attach = []provider.Image{{MediaType: "image/png", Data: []byte("x")}}
	a.Env.Vision = false // e.g. /model switched to a text-only model
	if _, err := a.Run(context.Background(), "what is this"); err != nil {
		t.Fatal(err)
	}
	sent := s.reqs[0].Messages[0]
	if len(sent.Images) != 0 || !strings.Contains(sent.Text, "1 image(s) omitted") {
		t.Fatalf("sent %+v", sent)
	}
	if len(a.Messages[0].Images) != 1 {
		t.Fatal("history must keep the image for a later vision model")
	}
}

func TestLessonFromErrorToFix(t *testing.T) {
	var l Ledger
	l.record("bash", json.RawMessage(`{"cmd":"go test ./calc"}`), "--- FAIL: TestAdd\ncalc_test.go:9: got -1 want 3\nFAIL\n[exit 1]", nil)
	l.record("edit", json.RawMessage(`{"path":"calc/add.go"}`), "ok", nil)
	l.record("bash", json.RawMessage(`{"cmd":"go test ./calc"}`), "ok", nil)
	ls := l.Lessons()
	if len(ls) != 1 || !strings.Contains(ls[0], "`go test ./calc` failed (--- FAIL: TestAdd)") || !strings.Contains(ls[0], "after changing calc/add.go") {
		t.Fatalf("lessons: %v", ls)
	}
}

func TestEffortEscalatesOnFailureAndResets(t *testing.T) {
	fail := calls(tc("x", "bash", `{"cmd":"exit 1"}`))
	var efforts []string
	step := func(req provider.Request) (provider.Response, error) {
		efforts = append(efforts, req.Reasoning.Effort)
		return fail(req)
	}
	s := &script{steps: []func(provider.Request) (provider.Response, error){step, step, step, step, func(req provider.Request) (provider.Response, error) {
		efforts = append(efforts, req.Reasoning.Effort)
		return provider.Response{Text: "done"}, nil
	}}}
	a := newAgent(t, s)
	a.Reasoning = provider.Reasoning{Effort: "medium", Efforts: []string{"low", "medium", "high", "max"}}
	var notes []string
	a.Events.Notice = func(m string) { notes = append(notes, m) }
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// Three failures in a row, then the stuck warning on the 4th identical call.
	if strings.Join(efforts, ",") != "medium,medium,medium,high,max" {
		t.Fatalf("efforts per call: %v", efforts)
	}
	if a.Reasoning.Effort != "medium" {
		t.Fatalf("effort not reset after the turn: %s", a.Reasoning.Effort)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, "|"), "thinking harder (effort high)") {
		t.Fatalf("notice: %v", notes)
	}
	if (provider.Reasoning{Effort: "max", Efforts: []string{"max"}}).NextEffort() != "" || (provider.Reasoning{}).NextEffort() != "" {
		t.Fatal("no level above max / no levels at all")
	}
}

func TestSubAgents(t *testing.T) {
	// The fake model: the main agent delegates two tasks in one turn; each
	// sub-agent reads a file and reports; then the main agent answers.
	var mu sync.Mutex
	subCalls := 0
	client := clientFunc(func(req provider.Request) (provider.Response, error) {
		first := req.Messages[0].Text
		last := req.Messages[len(req.Messages)-1]
		if strings.HasPrefix(first, "[sub-agent]") {
			mu.Lock()
			subCalls++
			mu.Unlock()
			n := "2"
			if strings.Contains(first, "look 1") {
				n = "1"
				if !strings.Contains(first, "plan mode") {
					t.Error("explore sub-agent should run in plan mode")
				}
			}
			if last.Role == provider.RoleUser {
				return provider.Response{ToolCalls: []provider.ToolCall{tc("r"+n, "read", `{"path":"a.txt"}`)}}, nil
			}
			return provider.Response{Text: "report: a.txt says hi (" + n + ")"}, nil
		}
		if last.Role == provider.RoleUser {
			return provider.Response{ToolCalls: []provider.ToolCall{
				tc("t1", "task", `{"prompt":"look 1","explore":true}`), tc("t2", "task", `{"prompt":"look 2"}`)}}, nil
		}
		return provider.Response{Text: "done"}, nil
	})
	a := newAgent(t, &script{})
	a.Client = client
	a.Tools = append(a.Tools, a.TodoTool(), a.TaskTool())
	os.WriteFile(filepath.Join(a.Env.Root, "a.txt"), []byte("hi"), 0o644)
	var subTools []string
	a.Events.SubToolStart = func(c provider.ToolCall) { mu.Lock(); subTools = append(subTools, c.Name); mu.Unlock() }
	if _, err := a.Run(context.Background(), "investigate"); err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, m := range a.Messages {
		if m.Role == provider.RoleTool {
			results = append(results, m.Text)
		}
	}
	joined := strings.Join(results, "\n")
	if len(results) != 2 || !strings.Contains(joined, "report: a.txt says hi (1)") || !strings.Contains(joined, "report: a.txt says hi (2)") ||
		!strings.Contains(joined, "read-only") || !strings.Contains(joined, "read-write") {
		t.Fatalf("task results: %v", results)
	}
	if subCalls != 4 || len(subTools) != 2 {
		t.Fatalf("sub calls %d, sub tools %v", subCalls, subTools)
	}
	// The parent's context holds only the reports, not the file contents.
	for _, m := range a.Messages {
		if m.Role == provider.RoleTool && strings.HasPrefix(m.Text, "hi") {
			t.Fatal("sub-agent tool output leaked into the parent context")
		}
	}
}

type clientFunc func(provider.Request) (provider.Response, error)

func (f clientFunc) Stream(_ context.Context, req provider.Request, _ func(string)) (provider.Response, error) {
	return f(req)
}

func TestKeepRawBlocksOnTruncation(t *testing.T) {
	raw := json.RawMessage(`[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"a","name":"read","input":{}},{"type":"tool_use","id":"b","name":"read","input":{}}]`)
	if !lastBlockIsToolUse(raw) {
		t.Fatal("last block is a tool_use")
	}
	got := string(keepRawBlocks(raw, []provider.ToolCall{{ID: "a"}}))
	if !strings.Contains(got, `"thinking"`) || !strings.Contains(got, `"id":"a"`) || strings.Contains(got, `"id":"b"`) {
		t.Fatalf("got %s", got)
	}
	if !contextOverflow(errors.New("api error 400: prompt is too long: 210000 tokens > 200000 maximum")) {
		t.Fatal("overflow not recognized")
	}
}

func TestSubAgentReportsWhenOutOfSteps(t *testing.T) {
	// The sub-agent keeps reading until its step limit, then is asked for
	// a report; a tool call at that point must not run.
	var reads int
	client := clientFunc(func(req provider.Request) (provider.Response, error) {
		first := req.Messages[0].Text
		last := req.Messages[len(req.Messages)-1]
		if strings.HasPrefix(first, "[sub-agent]") {
			if last.Role == provider.RoleUser && strings.Contains(last.Text, "used all your steps") {
				return provider.Response{Text: "report: read a.txt many times, not finished"}, nil
			}
			reads++
			return provider.Response{ToolCalls: []provider.ToolCall{tc(fmt.Sprintf("r%d", reads), "read", fmt.Sprintf(`{"path":"a.txt","offset":%d}`, reads))}}, nil
		}
		if last.Role == provider.RoleUser {
			return provider.Response{ToolCalls: []provider.ToolCall{tc("t1", "task", `{"prompt":"loop"}`)}}, nil
		}
		return provider.Response{Text: "done"}, nil
	})
	a := newAgent(t, &script{})
	a.Client = client
	a.Tools = append(a.Tools, a.TaskTool())
	os.WriteFile(filepath.Join(a.Env.Root, "a.txt"), []byte("hi\nthere\n"), 0o644)
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var result string
	for _, m := range a.Messages {
		if m.Role == provider.RoleTool {
			result = m.Text
		}
	}
	if !strings.Contains(result, "report: read a.txt many times") || reads != subMaxTurns {
		t.Fatalf("reads %d, result: %s", reads, result)
	}
}

func TestVerifyGateCatchesFailingCheck(t *testing.T) {
	// A test run that fails does not count as verification: the agent is
	// asked again, with the task quoted back.
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "edit", `{"path":"main.go","new":"package main\n"}`)),
		calls(tc("2", "bash", `{"cmd":"echo FAIL; exit 1 # go test"}`)),
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "Done."}, nil },
		calls(tc("3", "bash", `{"cmd":"echo ok # go test"}`)),
		func(provider.Request) (provider.Response, error) {
			return provider.Response{Text: "Fixed and verified."}, nil
		},
	}}
	a := newAgent(t, s)
	a.Verify = true
	st, err := a.Run(context.Background(), "make the build pass")
	if err != nil || st.Turns != 5 {
		t.Fatalf("turns=%d err=%v", st.Turns, err)
	}
	nudge := s.reqs[3].Messages[len(s.reqs[3].Messages)-1].Text
	if !strings.Contains(nudge, "latest build/test/lint run failed") || !strings.Contains(nudge, "make the build pass") {
		t.Fatalf("nudge: %q", nudge)
	}
}

func TestManyEditsToOneFileAskToStepBack(t *testing.T) {
	var steps []func(provider.Request) (provider.Response, error)
	for i := 0; i < fileEditWarn; i++ {
		steps = append(steps, calls(tc(fmt.Sprint(i), "edit", fmt.Sprintf(`{"path":"notes.txt","new":"v%d\n"}`, i))))
	}
	steps = append(steps, func(provider.Request) (provider.Response, error) { return provider.Response{Text: "ok"}, nil })
	a := newAgent(t, &script{steps: steps})
	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range a.Messages {
		if m.Role == provider.RoleTool && strings.Contains(m.Text, "Step back") {
			found = true
		}
	}
	if !found {
		t.Fatal("no step-back note after many edits to one file")
	}
}

func TestDetectProject(t *testing.T) {
	dir := t.TempDir()
	if detectProject(dir) != "" {
		t.Fatal("empty dir")
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"node --test"}}`), 0o644)
	if got := detectProject(dir); got != `project=go+node tests="go test ./...; npm test"` {
		t.Fatalf("got %s", got)
	}
}

func TestOpenTodosNudgeOnce(t *testing.T) {
	s := &script{steps: []func(provider.Request) (provider.Response, error){
		calls(tc("1", "todo", `{"items":[{"text":"write code","status":"done"},{"text":"write docs","status":"pending"}]}`)),
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "Done."}, nil },
		func(provider.Request) (provider.Response, error) { return provider.Response{Text: "Docs skipped on purpose."}, nil },
	}}
	a := newAgent(t, s)
	a.Tools = append(a.Tools, a.TodoTool())
	st, err := a.Run(context.Background(), "x")
	if err != nil || st.Turns != 3 {
		t.Fatalf("turns=%d err=%v", st.Turns, err)
	}
	if last := s.reqs[2].Messages[len(s.reqs[2].Messages)-1].Text; !strings.Contains(last, "write docs") {
		t.Fatalf("nudge: %q", last)
	}
}
