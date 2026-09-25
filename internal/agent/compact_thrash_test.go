package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/tegarthegreat/agentium/internal/provider"
)

type countingSummarizer struct{ calls int }

func (c *countingSummarizer) Stream(ctx context.Context, req provider.Request, onText func(string)) (provider.Response, error) {
	c.calls++
	return provider.Response{Text: strings.Repeat("summary ", 1500), StopReason: "stop"}, nil // ~12k chars
}

// One long run (a single user message) with a small window and reasoning
// on every step must not summarize on every step: this used to compact
// 29 times in 40 steps and never get work done.
func TestNoCompactionThrashInOneRun(t *testing.T) {
	sum := &countingSummarizer{}
	a := &Agent{Client: sum, ContextTokens: 60000, System: strings.Repeat("s", 6000)}
	a.Messages = []provider.Message{{Role: provider.RoleUser, Text: "fix the bugs"}}
	_, compactAt := a.budgets()
	for step := 0; step < 40; step++ {
		a.Messages = append(a.Messages,
			provider.Message{Role: provider.RoleAssistant, Reasoning: strings.Repeat("r", 5000),
				ToolCalls: []provider.ToolCall{{ID: "c", Name: "read", Args: []byte(`{"path":"more.py","offset":1,"limit":60}`)}}},
			provider.Message{Role: provider.RoleTool, ToolCallID: "c", Text: strings.Repeat("x", 4000)})
		a.manageContext(context.Background(), false)
		if a.size() > compactAt {
			t.Fatalf("step %d: still over the window after managing it (%d > %d)", step, a.size(), compactAt)
		}
	}
	if sum.calls > 3 {
		t.Fatalf("%d summaries in 40 steps", sum.calls)
	}
	t.Logf("%d summaries in 40 steps", sum.calls)
}

// When only a summary helps (long assistant text), one summary must buy
// many steps.
func TestCompactionLeavesRoom(t *testing.T) {
	sum := &countingSummarizer{}
	a := &Agent{Client: sum, ContextTokens: 60000, System: strings.Repeat("s", 6000)}
	a.Messages = []provider.Message{{Role: provider.RoleUser, Text: "write the docs"}}
	for step := 0; step < 40; step++ {
		a.Messages = append(a.Messages,
			provider.Message{Role: provider.RoleAssistant, Text: strings.Repeat("t", 6000), Reasoning: strings.Repeat("r", 3000),
				ToolCalls: []provider.ToolCall{{ID: "c", Name: "bash", Args: []byte(`{"command":"true"}`)}}},
			provider.Message{Role: provider.RoleTool, ToolCallID: "c", Text: "ok"})
		a.manageContext(context.Background(), false)
	}
	if sum.calls > 8 {
		t.Fatalf("%d summaries in 40 steps", sum.calls)
	}
	t.Logf("%d summaries in 40 steps", sum.calls)
}

func TestCompactionFactsFiltered(t *testing.T) {
	var got []string
	a := &Agent{OnRemember: func(f string) { got = append(got, f) }}
	a.Messages = []provider.Message{{Role: provider.RoleUser, Text: "Fix the 12 bugs in more_itertools. Only edit files under more_itertools/."}}
	a.rememberFacts([]string{
		"Only edit files under more_itertools/",             // the task's constraint
		"Tests run with: python -m pytest -q tests/",        // a real convention
		"Tests run with: python -m pytest -q tests/",        // repeated
		"The project pins Python 3.9 in tox.ini",            // real
		"Doc builds need sphinx installed via requirements", // real
		"CI caches the venv under .cache/venv",              // over the cap
	})
	a.rememberFacts([]string{"The project pins Python 3.9 in tox.ini"}) // a later summary repeats it
	want := "Tests run with: python -m pytest -q tests/|The project pins Python 3.9 in tox.ini|Doc builds need sphinx installed via requirements"
	if strings.Join(got, "|") != want {
		t.Fatalf("remembered %q", got)
	}
}
