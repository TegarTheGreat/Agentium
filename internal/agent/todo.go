package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/tool"
)

const maxTodos = 12

// TodoTool lets the model keep a task list in the ledger. The list is
// working memory: it survives compaction verbatim.
func (a *Agent) TodoTool() tool.Tool {
	return tool.Tool{
		Def: provider.ToolDef{
			Name:        "todo",
			Description: "Task list for work with 3+ steps (skip for small tasks). Send the whole list each time; mark one item in_progress, then done.",
			Schema:      json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array","items":{"type":"object","required":["text","status"],"properties":{"text":{"type":"string"},"status":{"enum":["pending","in_progress","done"]}}}}}}`),
		},
		Run: func(ctx context.Context, _ *tool.Env, raw json.RawMessage) (string, error) {
			var in struct {
				Items []Todo `json:"items"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %v", err)
			}
			if len(in.Items) > maxTodos {
				return "", fmt.Errorf("at most %d items; merge small steps", maxTodos)
			}
			for i, t := range in.Items {
				switch t.Status {
				case "pending", "in_progress", "done":
				default:
					return "", fmt.Errorf("item %d: status must be pending, in_progress or done", i+1)
				}
				if t.Text == "" {
					return "", errors.New("empty item")
				}
				in.Items[i].Text = oneLine(t.Text, 160)
			}
			running(ctx, a).Ledger.SetTodos(in.Items)
			if len(in.Items) == 0 {
				return "(todo list cleared)", nil
			}
			return renderTodos(in.Items), nil
		},
	}
}
