package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/tool"
)

// Oracle is a stronger model the agent can consult (config
// "oracle_model"), as in Amp: for planning, reviewing a design or getting
// unstuck, while the everyday work runs on a faster, cheaper model.
type Oracle struct {
	Client    provider.Client
	Model     string
	Cost      func(provider.Usage) float64
	Reasoning provider.Reasoning
	MaxOutput int
}

const (
	oracleFileMax  = 60 * 1024
	oracleFilesMax = 200 * 1024
)

const oracleSystem = `You are the oracle: a senior engineer a coding agent consults on hard problems (planning a change, reviewing a design or diff, finding the cause of a bug it is stuck on). Think carefully, then answer concretely and briefly: the answer or plan, the reasons, and what to check. Point to exact files, functions and lines. You cannot run tools; say what the agent should verify.`

// OracleTool asks the oracle a question, with files attached.
func (a *Agent) OracleTool() tool.Tool {
	return tool.Tool{
		Def: provider.ToolDef{
			Name: "oracle",
			Description: "Ask a stronger model for a second opinion: planning a non-trivial change, reviewing a design or diff, or a bug you are stuck on after two attempts. " +
				"Give the question and the relevant files (paths); it cannot see anything else. Costs more: not for simple questions.",
			Schema: json.RawMessage(`{"type":"object","required":["question"],"properties":{"question":{"type":"string","description":"what you want to know, with the context it needs (what you tried, errors)"},"files":{"type":"array","items":{"type":"string"},"description":"paths of files to show it"}}}`),
		},
		Run: func(ctx context.Context, env *tool.Env, raw json.RawMessage) (string, error) {
			o := a.Oracle
			if o == nil {
				return "", errors.New("no oracle_model is configured")
			}
			var in struct {
				Question string   `json:"question"`
				Files    []string `json:"files"`
			}
			if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Question) == "" {
				return "", errors.New("question is required")
			}
			var sb strings.Builder
			var skipped []string
			for _, f := range in.Files {
				p := f
				if !filepath.IsAbs(p) && env != nil {
					p = filepath.Join(env.Root, p)
				}
				if env != nil && env.Gate != nil {
					if ok, _ := env.Gate.Read(p); !ok {
						skipped = append(skipped, f+" (not allowed)")
						continue
					}
				}
				b, err := os.ReadFile(p)
				switch {
				case err != nil:
					skipped = append(skipped, f+" ("+err.Error()+")")
				case len(b) > oracleFileMax || sb.Len()+len(b) > oracleFilesMax:
					skipped = append(skipped, f+" (too big; quote the relevant part in the question)")
				default:
					fmt.Fprintf(&sb, "<file path=%q>\n%s\n</file>\n", f, strings.ReplaceAll(string(b), "</file>", "<\\/file>"))
				}
			}
			msg := sb.String() + "\n" + in.Question
			resp, err := o.Client.Stream(ctx, provider.Request{
				Model: o.Model, System: oracleSystem, Reasoning: o.Reasoning, MaxOutput: o.MaxOutput, MaxTokens: 8000,
				Messages: []provider.Message{{Role: provider.RoleUser, Text: msg}},
			}, nil)
			if err != nil {
				return "", fmt.Errorf("oracle: %w", err)
			}
			cost := 0.0
			if o.Cost != nil {
				cost = o.Cost(resp.Usage)
			}
			a.mu.Lock()
			a.Usage.Add(resp.Usage)
			a.Spent += cost
			a.mu.Unlock()
			out := strings.TrimSpace(resp.Text)
			if out == "" {
				out = "(the oracle gave no answer)"
			}
			if len(skipped) > 0 {
				out += "\n\n[not shown to the oracle: " + strings.Join(skipped, ", ") + "]"
			}
			return out, nil
		},
	}
}
