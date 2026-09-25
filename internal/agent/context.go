package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// Characters per token used for budgeting. Code and JSON tokenize denser
// than prose, so this errs on the side of acting early.
const charsPerToken = 3

// imageChars is the budget charge for one image (~1.6k tokens).
const imageChars = 1600 * charsPerToken

const (
	keepRecentTools = 6
	elidedKeep      = 300
	elideArgOver    = 400
)

// budgets returns (elide at, compact at) in characters.
func (a *Agent) budgets() (int, int) {
	if a.ContextTokens > 0 {
		reserve := a.MaxTokens
		if reserve <= 0 {
			reserve = 16000
		}
		usable := (a.ContextTokens - reserve) * charsPerToken
		if usable < 20000 {
			usable = 20000
		}
		return usable * 55 / 100, usable * 85 / 100
	}
	return a.ContextChars, 0
}

// size estimates the conversation size in characters.
func (a *Agent) size() int {
	n := len(a.System)
	for _, m := range a.Messages {
		n += msgSize(m)
	}
	for _, t := range a.Tools {
		n += len(t.Def.Description) + len(t.Def.Schema)
	}
	return n
}

// msgSize estimates what one message costs in the request, in characters.
func msgSize(m provider.Message) int {
	n := len(m.Text) + len(m.Images)*imageChars + len(m.Reasoning)
	if len(m.Raw) > 0 {
		// Replayed provider blocks (thinking, full tool inputs) are what
		// is really sent; they overlap Text and ToolCalls.
		return n + len(m.Raw)
	}
	for _, c := range m.ToolCalls {
		n += len(c.Args) + len(c.Name)
	}
	return n
}

// manageContext keeps the conversation within the model's window:
// first elide old tool output, old edit payloads and old reasoning (no
// LLM call, done in one batch with headroom so it rarely re-runs and the
// cached prefix stays stable), then, if still too big, summarize the
// older part. Routine elision runs at turn boundaries; inside one long
// run it runs only when the window is nearly full, before summarizing.
func (a *Agent) manageContext(ctx context.Context, boundary bool) {
	elideAt, compactAt := a.budgets()
	if elideAt > 0 && a.size() > elideAt && (boundary || compactAt > 0 && a.size() > compactAt) {
		a.elide(keepRecentTools)
		if a.size() > elideAt*80/100 {
			a.elide(2) // leave headroom so the next turns don't re-elide
		}
	}
	if compactAt > 0 && a.size() > compactAt {
		if err := a.compact(ctx); err != nil {
			a.notice("compaction failed (" + err.Error() + "); eliding harder")
			a.elide(2)
		} else if a.size() > compactAt*70/100 {
			// The summary left too little room: without more cut, the
			// next step would summarize again (and again).
			a.elide(2)
		}
	}
}

// elide shortens tool outputs and large edit arguments older than the
// most recent keep tool results.
func (a *Agent) elide(keep int) {
	seen, first := 0, -1
	for i := len(a.Messages) - 1; i >= 0; i-- {
		m := &a.Messages[i]
		switch m.Role {
		case provider.RoleTool:
			seen++
			if seen <= keep {
				continue
			}
			note := ""
			if len(m.Images) > 0 {
				note = fmt.Sprintf("\n[%d old image(s) elided; read again if needed]", len(m.Images))
				m.Images = nil
				first = i
			}
			if len(m.Text) > elidedKeep+100 {
				cut := len(m.Text) - elidedKeep
				m.Text = strings.ToValidUTF8(m.Text[:elidedKeep], "") + fmt.Sprintf("\n[elided %d chars of old output; rerun the tool if needed]", cut)
				first = i
			}
			m.Text += note
		case provider.RoleUser:
			// Old screenshots cost ~1.6k tokens on every request.
			if seen > keep && len(m.Images) > 0 {
				m.Text += fmt.Sprintf("\n[%d old image(s) elided]", len(m.Images))
				m.Images = nil
				first = i
			}
		case provider.RoleAssistant:
			if seen <= keep {
				continue
			}
			// Old steps' reasoning is not needed again (providers accept
			// history without it) and is often the largest part.
			if m.Reasoning != "" {
				m.Reasoning = ""
				first = i
			}
			shortened := false
			for j := range m.ToolCalls {
				if e := elideArgs(m.ToolCalls[j].Args); len(e) != len(m.ToolCalls[j].Args) {
					m.ToolCalls[j].Args = e
					first = i
					shortened = true
				}
			}
			if shortened {
				// Raw would replay the full arguments; without it the
				// shortened calls are sent (an old turn's thinking block is
				// not needed).
				m.Raw = nil
			}
		}
	}
	if first >= 0 {
		a.invalidateFrom(first)
		if a.Env != nil {
			a.Env.ForgetReads() // elided reads must be served in full again
		}
	}
}

// invalidateFrom drops signed thinking blocks that an edit at message i
// invalidated: only models that bind blocks to the conversation prefix
// are affected. The in-flight assistant turn (the last assistant message
// when a tool round is open) keeps its blocks, since the API expects them
// on the turn it is continuing; on the Claude API, drop_block covers it.
// Messages keep their text and tool calls, so the model loses nothing.
func (a *Agent) invalidateFrom(i int) {
	inflight := -1
	if n := len(a.Messages); n > 0 && a.Messages[n-1].Role != provider.RoleUser {
		for j := n - 1; j >= 0; j-- {
			if a.Messages[j].Role == provider.RoleAssistant {
				inflight = j
				break
			}
		}
	}
	for j := i; j < len(a.Messages); j++ {
		if j != inflight && a.Messages[j].Raw != nil && provider.PreservedThinking(a.Messages[j].RawModel) {
			a.Messages[j].Raw = nil
		}
	}
}

// elideArgs shortens long string arguments (file contents in old edits).
func elideArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) <= elideArgOver {
		return raw
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	changed := false
	var shorten func(v any) any
	shorten = func(v any) any {
		switch x := v.(type) {
		case string:
			if len(x) > elideArgOver {
				changed = true
				return strings.ToValidUTF8(x[:120], "") + fmt.Sprintf("…[elided %d chars]", len(x)-120)
			}
		case map[string]any: // e.g. each of edit's edits
			for k, e := range x {
				x[k] = shorten(e)
			}
		case []any:
			for i, e := range x {
				x[i] = shorten(e)
			}
		}
		return v
	}
	for k, v := range m {
		m[k] = shorten(v)
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

const compactPrompt = `Summarize this coding-agent conversation so the work can continue without it.
Include: the user's goals and constraints, decisions made and why, files created or changed, the current state (what works, what is failing), and the next steps.
Be specific: exact paths, names, commands, error messages. No preamble.
After the summary, list at most 3 durable facts worth remembering beyond this session (project conventions, user preferences, lessons learned the hard way) as lines starting with "@remember ". Not the task itself, its constraints or its progress: those belong to this conversation only. Omit if none.`

// maxFactsPerCompaction bounds what one summary adds to long-term memory.
const maxFactsPerCompaction = 3

// rememberFacts passes on the durable facts a summary surfaced, minus
// ones already passed this session and ones that only restate what the
// user asked (a long run compacting often would otherwise fill memory
// with copies of its own task).
func (a *Agent) rememberFacts(facts []string) {
	if a.OnRemember == nil {
		return
	}
	var said strings.Builder
	for _, m := range a.Messages {
		if m.Role == provider.RoleUser {
			said.WriteString(strings.ToLower(m.Text) + " ")
		}
	}
	saidWords := wordSet(said.String())
	n := 0
	for _, f := range facts {
		key := strings.ToLower(f)
		if n == maxFactsPerCompaction || a.remembered[key] {
			continue
		}
		fw := wordSet(key)
		common := 0
		for w := range fw {
			if saidWords[w] {
				common++
			}
		}
		if len(fw) > 0 && common*100/len(fw) >= 70 {
			continue // mostly the user's own words: the task, not a lesson
		}
		if a.remembered == nil {
			a.remembered = map[string]bool{}
		}
		a.remembered[key] = true
		a.OnRemember(f)
		n++
	}
}

// wordSet is the set of words of 3+ letters or digits in s.
func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		if len(w) >= 3 {
			out[w] = true
		}
	}
	return out
}

// compact replaces the older part of the conversation with a summary.
func (a *Agent) compact(ctx context.Context) error {
	_, compactAt := a.budgets()
	keepBudget := compactAt * 35 / 100
	// Walk back to find where the kept tail starts: at a real user message
	// (not a tool result), so tool calls and results stay paired.
	// The kept tail may start at a real user message or at an assistant
	// message (its tool results follow it, so calls stay paired); this
	// also works inside one long run with a single user message.
	split, kept := -1, 0
	for i := len(a.Messages) - 1; i > 0; i-- {
		m := a.Messages[i]
		kept += msgSize(m) // with reasoning: sized like the budget is
		if m.Role == provider.RoleUser || m.Role == provider.RoleAssistant {
			if i == len(a.Messages)-1 {
				continue // keep at least one full exchange in the tail
			}
			split = i
			if kept >= keepBudget {
				break
			}
		}
	}
	if split <= 0 {
		return fmt.Errorf("the conversation is too short to summarize")
	}
	tr := transcript(a.Messages[:split])
	client, model := a.Client, a.Model
	if a.Fast != nil {
		client, model = a.Fast, a.FastModel
	}
	a.notice("compacting older conversation")
	resp, err := client.Stream(ctx, provider.Request{
		Model:  model,
		System: "You compress coding-agent conversations into precise working notes.",
		// The most recent history matters most: keep the end if too long.
		Messages:  []provider.Message{{Role: provider.RoleUser, Text: clipHead(tr, 400000) + "\n\n---\n" + compactPrompt}},
		MaxTokens: 4000,
	}, nil)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.Usage.Add(resp.Usage)
	if a.Cost != nil {
		a.Spent += a.Cost(resp.Usage)
	}
	a.mu.Unlock()
	summary := stripToolMarkup(resp.Text)
	if summary == "" {
		return fmt.Errorf("empty summary")
	}
	var kept2, facts []string
	for _, line := range strings.Split(summary, "\n") {
		if f, ok := strings.CutPrefix(strings.TrimSpace(line), "@remember "); ok {
			if f = strings.TrimSpace(f); f != "" {
				facts = append(facts, f)
			}
			continue
		}
		kept2 = append(kept2, line)
	}
	a.rememberFacts(facts)
	summary = strings.TrimSpace(strings.Join(kept2, "\n"))
	tail := append([]provider.Message(nil), a.Messages[split:]...)
	text := "[Summary of the earlier conversation]\n" + summary
	// The user's own words survive verbatim: a paraphrase loses details.
	var asks []string
	for _, m := range a.Messages[:split] {
		if m.Role == provider.RoleUser && m.Text != "" && !strings.HasPrefix(m.Text, "[agentium]") && !strings.HasPrefix(m.Text, "[Summary of the earlier") {
			// Recalled memory and hook output are not the user's words.
			if t := provider.UserWords(m.Text); t != "" {
				asks = append(asks, clip(t, 2000))
			}
		}
	}
	if n := len(asks); n > 0 {
		asks = asks[max(n-2, 0):]
		text += "\n\n[The user's latest requests before this point, verbatim]\n" + strings.Join(asks, "\n---\n")
	}
	if st := a.Ledger.Render(); st != "" {
		text += "\n\n" + st
	}
	a.Messages = append([]provider.Message{{Role: provider.RoleUser, Text: text}}, tail...)
	if a.Env != nil {
		a.Env.ForgetReads()
	}
	a.invalidateFrom(0)
	return nil
}

// stripToolMarkup cuts tool-call markup a model wrote as text (seen
// when the transcript it summarizes shows tool calls): from the first line
// that starts with such markup, outside a code fence.
func stripToolMarkup(s string) string {
	lines := strings.Split(s, "\n")
	fence := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		for _, m := range []string{"<tool_calls>", "<function_calls>", "<invoke ", "<tool_call>", "<｜tool", "<|tool"} {
			if strings.HasPrefix(t, m) {
				return strings.TrimSpace(strings.Join(lines[:i], "\n"))
			}
		}
	}
	return strings.TrimSpace(s)
}

// transcript renders messages as plain text for a summarizing model.
func transcript(msgs []provider.Message) string {
	var tr strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleUser:
			fmt.Fprintf(&tr, "\nUSER: %s\n", m.Text)
			if len(m.Images) > 0 {
				fmt.Fprintf(&tr, "[user attached %d image(s)]\n", len(m.Images))
			}
		case provider.RoleAssistant:
			if m.Text != "" {
				fmt.Fprintf(&tr, "\nASSISTANT: %s\n", m.Text)
			}
			for _, c := range m.ToolCalls {
				fmt.Fprintf(&tr, "TOOL CALL %s %s\n", c.Name, clip(string(c.Args), 400))
			}
		case provider.RoleTool:
			fmt.Fprintf(&tr, "RESULT: %s\n", clip(m.Text, 600))
		}
	}
	return tr.String()
}

const handoffPrompt = `Write the first message of a NEW session with a coding agent that continues this work with a fresh context. The new session's goal: %s

Write it as the user, in the user's language, addressed to the agent. Include only what that goal needs: the goal itself, the relevant files (paths), decisions already made and why, the current state (what works, what fails, exact errors), constraints the user gave, and the first steps. Be specific and brief (at most about 30 lines). Plain text only: do not call tools or write tool-call markup. No preamble.`

// Handoff writes the opening message for a new session that continues
// this one toward goal, with only the context the goal needs (Amp's
// handoff: a fresh start instead of a lossy compaction).
func (a *Agent) Handoff(ctx context.Context, goal string) (string, error) {
	if goal == "" {
		goal = "carry on with the current task"
	}
	client, model := a.Client, a.Model
	if a.Fast != nil {
		client, model = a.Fast, a.FastModel
	}
	resp, err := client.Stream(ctx, provider.Request{
		Model:     model,
		System:    "You write precise hand-off briefs between coding-agent sessions.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Text: clipHead(transcript(a.Messages), 300000) + "\n\n---\n" + fmt.Sprintf(handoffPrompt, goal)}},
		MaxTokens: 4000,
	}, nil)
	if err != nil {
		return "", err
	}
	a.Charge(resp.Usage, a.Fast == nil)
	brief := stripToolMarkup(resp.Text)
	if brief == "" {
		return "", fmt.Errorf("the model wrote nothing")
	}
	return brief, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// clipHead keeps the last n bytes of s.
func clipHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "[earlier part omitted]\n" + strings.ToValidUTF8(s[len(s)-n:], "")
}

// Compact summarizes the older part of the conversation now (the /compact
// command); the recent exchange is kept verbatim.
func (a *Agent) Compact(ctx context.Context) error { return a.compact(ctx) }

// ContextBreakdown estimates, in tokens, what the next request carries:
// the system prompt, the tool definitions and the conversation.
func (a *Agent) ContextBreakdown() (system, tools, conversation int) {
	system = len(a.System) / charsPerToken
	for _, t := range a.Tools {
		tools += len(t.Def.Name) + len(t.Def.Description) + len(t.Def.Schema)
	}
	tools /= charsPerToken
	conversation = max(a.size()/charsPerToken-system-tools, 0)
	return system, tools, conversation
}
