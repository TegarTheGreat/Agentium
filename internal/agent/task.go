package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/tool"
)

const (
	subMaxTurns  = 40
	subReportMax = 6000
)

const subBrief = `[sub-agent] You are a sub-agent working for another agent, with a fresh context. Do the task below yourself; do not ask questions, decide sensibly. When done, reply with a concise report for the other agent: what you found or changed (exact paths and line numbers), how you verified it, and anything left undone.

Task: `

// TaskTool lets the model hand a self-contained subtask to a sub-agent
// with a fresh context window. Exploration that reads twenty files, or a
// well-specified change, then costs the main conversation only the
// report. Several task calls in one turn run in parallel.
func (a *Agent) TaskTool() tool.Tool {
	return tool.Tool{
		Def: provider.ToolDef{
			Name:        "task",
			Description: "Delegate a self-contained subtask (broad investigation, or a well-specified change) to a sub-agent with a fresh context; returns a short report. Calls in one turn run in parallel. explore=true: read-only.",
			Schema:      json.RawMessage(`{"type":"object","required":["prompt"],"properties":{"prompt":{"type":"string"},"explore":{"type":"boolean"}}}`),
		},
		Run: func(ctx context.Context, env *tool.Env, raw json.RawMessage) (string, error) {
			var in struct {
				Prompt  string `json:"prompt"`
				Explore bool   `json:"explore"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %v", err)
			}
			if in.Prompt == "" {
				return "", errors.New("prompt is required")
			}
			parent := running(ctx, a)
			if parent.depth > 0 {
				return "", errors.New("a sub-agent cannot start sub-agents; do the work directly")
			}
			return parent.runSub(ctx, env, in.Prompt, in.Explore)
		},
	}
}

func (a *Agent) runSub(ctx context.Context, env *tool.Env, prompt string, explore bool) (string, error) {
	gate := env.Gate
	if explore || gate != nil && gate.GetMode() == policy.Plan {
		g := &policy.Gate{Mode: policy.Plan}
		if gate != nil {
			g.Root, g.Approve = gate.Root, gate.Approve
		}
		gate = g
	}
	childEnv := env.Child(gate)
	defer childEnv.KillJobs()
	sub := &Agent{
		Client: a.Client, Model: a.Model,
		// Same system prompt and tools as the parent: the provider's
		// prompt cache is reused (the task tool refuses at depth 1).
		System: a.System, Tools: a.Tools, Env: childEnv,
		MaxTurns: subMaxTurns, MaxTokens: a.MaxTokens, ContextTokens: a.ContextTokens, ContextChars: a.ContextChars,
		Fast: a.Fast, FastModel: a.FastModel, Verify: !explore, Reasoning: a.Reasoning, FastMode: a.FastMode,
		Cost: a.Cost, depth: a.depth + 1,
		Events: Events{Notice: a.Events.Notice, ToolStart: a.Events.SubToolStart, Retry: a.Events.Retry},
	}
	if a.MaxCost > 0 {
		sub.MaxCost = a.MaxCost - a.Spent
	}
	_, err := sub.Run(ctx, subBrief+prompt)
	a.mu.Lock()
	a.Usage.Add(sub.Usage)
	a.Spent += sub.Spent
	a.mu.Unlock()
	report := ""
	for i := len(sub.Messages) - 1; i >= 0; i-- {
		if m := sub.Messages[i]; m.Role == provider.RoleAssistant && m.Text != "" {
			report = m.Text
			break
		}
	}
	if len(report) > subReportMax {
		report = report[:subReportMax] + "\n[report truncated]"
	}
	if report == "" {
		report = "(the sub-agent produced no report)"
	}
	if err != nil {
		report += "\n[sub-agent stopped early: " + err.Error() + "]"
	}
	return fmt.Sprintf("[sub-agent: %d turns, %s]\n%s", sub.Turns, pickMode(explore), report), nil
}

func pickMode(explore bool) string {
	if explore {
		return "read-only"
	}
	return "read-write"
}
