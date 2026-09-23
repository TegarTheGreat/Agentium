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
		n += len(m.Text)
		for _, c := range m.ToolCalls {
			n += len(c.Args) + len(c.Name)
		}
	}
	return n
}

// manageContext keeps the conversation within the model's window:
// first elide old tool output and old edit payloads (no LLM call, done
// in one batch with headroom so it rarely re-runs and the cached prefix
// stays stable), then, if still too big, summarize the older part.
// Routine elision runs only at turn boundaries.
func (a *Agent) manageContext(ctx context.Context, boundary bool) {
	elideAt, compactAt := a.budgets()
	if boundary && elideAt > 0 && a.size() > elideAt {
		a.elide(keepRecentTools)
		if a.size() > elideAt*80/100 {
			a.elide(2) // leave headroom so the next turns don't re-elide
		}
	}
	if compactAt > 0 && a.size() > compactAt {
		if err := a.compact(ctx); err != nil {
			a.notice("compaction failed (" + err.Error() + "); eliding harder")
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
			if seen <= keep || len(m.Text) <= elidedKeep+100 {
				continue
			}
			cut := len(m.Text) - elidedKeep
			m.Text = strings.ToValidUTF8(m.Text[:elidedKeep], "") + fmt.Sprintf("\n[elided %d chars of old output; rerun the tool if needed]", cut)
			first = i
		case provider.RoleAssistant:
			if seen <= keep {
				continue
			}
			for j := range m.ToolCalls {
				if e := elideArgs(m.ToolCalls[j].Args); len(e) != len(m.ToolCalls[j].Args) {
					m.ToolCalls[j].Args = e
					first = i
				}
			}
		}
	}
	if first >= 0 {
		a.invalidateFrom(first)
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
	for k, v := range m {
		if s, ok := v.(string); ok && len(s) > elideArgOver {
			m[k] = strings.ToValidUTF8(s[:120], "") + fmt.Sprintf("…[elided %d chars]", len(s)-120)
			changed = true
		}
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
After the summary, list durable facts worth remembering beyond this session (project conventions, user preferences, lessons) as lines starting with "@remember ". Omit if none.`

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
		kept += len(m.Text)
		for _, c := range m.ToolCalls {
			kept += len(c.Args)
		}
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
		return fmt.Errorf("no safe split point")
	}
	var tr strings.Builder
	for _, m := range a.Messages[:split] {
		switch m.Role {
		case provider.RoleUser:
			fmt.Fprintf(&tr, "\nUSER: %s\n", m.Text)
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
	client, model := a.Client, a.Model
	if a.Fast != nil {
		client, model = a.Fast, a.FastModel
	}
	a.notice("compacting older conversation")
	resp, err := client.Stream(ctx, provider.Request{
		Model:     model,
		System:    "You compress coding-agent conversations into precise working notes.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Text: clip(tr.String(), 400000) + "\n\n---\n" + compactPrompt}},
		MaxTokens: 4000,
	}, nil)
	if err != nil {
		return err
	}
	a.Usage.Add(resp.Usage)
	summary := strings.TrimSpace(resp.Text)
	if summary == "" {
		return fmt.Errorf("empty summary")
	}
	var kept2 []string
	for _, line := range strings.Split(summary, "\n") {
		if f, ok := strings.CutPrefix(strings.TrimSpace(line), "@remember "); ok {
			if a.OnRemember != nil && strings.TrimSpace(f) != "" {
				a.OnRemember(strings.TrimSpace(f))
			}
			continue
		}
		kept2 = append(kept2, line)
	}
	summary = strings.TrimSpace(strings.Join(kept2, "\n"))
	tail := append([]provider.Message(nil), a.Messages[split:]...)
	a.Messages = append([]provider.Message{{Role: provider.RoleUser,
		Text: "[Summary of the earlier conversation]\n" + summary}}, tail...)
	a.invalidateFrom(0)
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
