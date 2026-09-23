package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tegarthegreat/agentium/internal/mcp"
)

// MCPTools wraps the tools of running MCP servers.
func MCPTools(clients []*mcp.Client) []Tool {
	var out []Tool
	for _, c := range clients {
		for _, t := range c.Tools {
			c, t := c, t
			schema := t.InputSchema
			if len(schema) == 0 || !json.Valid(schema) {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			desc := t.Description
			if len(desc) > 500 {
				desc = desc[:500] + "…"
			}
			out = append(out, Tool{
				Def: providerDef(mcp.ToolName(c.Name, t.Name), desc, string(schema)),
				Run: func(ctx context.Context, env *Env, args json.RawMessage) (string, error) {
					if env.Gate != nil {
						if ok, why := env.Gate.External(c.Name + "." + t.Name); !ok {
							return "", fmt.Errorf("denied (%s)", why)
						}
					}
					env.mutate() // MCP tools may change files: checkpoint first
					res, isErr, err := c.CallTool(ctx, t.Name, args)
					if err != nil {
						return "", err
					}
					res = Clip(res, bashMaxOutput)
					if isErr {
						return res, errors.New("tool reported an error")
					}
					return res, nil
				},
			})
		}
	}
	return out
}
