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
	subMaxTurns  = 60
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
			Description: "Delegate a self-contained subtask (broad investigation, or a well-specified change) to a sub-agent with a fresh context; returns a short report. Calls in one turn run in parallel. explore=true: read-only. title: a 3-6 word label shown to the user.",
			Schema:      json.RawMessage(`{"type":"object","required":["prompt"],"properties":{"title":{"type":"string"},"prompt":{"type":"string"},"explore":{"type":"boolean"}}}`),
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
		MaxTurns: subMaxTurns, MaxTokens: a.MaxTokens, MaxOutput: a.MaxOutput, ContextTokens: a.ContextTokens, ContextChars: a.ContextChars,
		Fast: a.Fast, FastModel: a.FastModel, Verify: !explore, Reasoning: a.Reasoning, FastMode: a.FastMode,
		Cost: a.Cost, depth: a.depth + 1,
		Events: Events{Notice: a.Events.Notice, Retry: a.Events.Retry, ToolStart: func(c provider.ToolCall) {
			if a.Events.SubToolStart != nil {
				a.Events.SubToolStart(c)
			}
			if a.Events.SubAgentTool != nil {
				a.Events.SubAgentTool(prompt, c)
			}
		}},
	}
	if a.MaxCost > 0 {
		a.mu.Lock()
		sub.MaxCost = a.MaxCost - a.Spent
		a.mu.Unlock()
	}
	_, err := sub.Run(ctx, subBrief+prompt)
	if (errors.Is(err, ErrMaxTurns) || errors.Is(err, ErrStuck)) && ctx.Err() == nil {
		// Out of steps: the work done so far is only useful with a report,
		// so ask for one. Tools stay declared (the history holds tool
		// calls and the prompt cache is reused) but no longer run.
		sub.MaxTurns, sub.Verify = 1, false
		sub.Tools = refuseAll(sub.Tools)
		sub.Run(ctx, "[agentium] You have used all your steps. Do not call tools. Reply now with your report: what is done (exact paths), what is verified, what is left undone and how to finish it.")
	}
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

// refuseAll keeps the tools' definitions but makes every call fail.
func refuseAll(tools []tool.Tool) []tool.Tool {
	out := make([]tool.Tool, len(tools))
	for i, t := range tools {
		out[i] = tool.Tool{Def: t.Def, Run: func(context.Context, *tool.Env, json.RawMessage) (string, error) {
			return "", errors.New("not run: step limit reached; write your report instead")
		}}
	}
	return out
}

func pickMode(explore bool) string {
	if explore {
		return "read-only"
	}
	return "read-write"
}
