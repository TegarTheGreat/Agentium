package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/mcp"
)

// MCPTools wraps the tools of running MCP servers.
// mcpCallTimeout bounds one MCP tool call.
var mcpCallTimeout = 10 * time.Minute

func MCPTools(clients []*mcp.Client) []Tool {
	var out []Tool
	for _, c := range clients {
		conn, server := &mcpConn{c: c}, c.Name
		for _, t := range c.Tools {
			t := t
			schema := t.InputSchema
			if len(schema) == 0 || !json.Valid(schema) {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			desc := t.Description
			if len(desc) > 500 {
				desc = desc[:500] + "…"
			}
			out = append(out, Tool{
				Def: providerDef(mcp.ToolName(server, t.Name), desc, string(schema)),
				Run: func(ctx context.Context, env *Env, args json.RawMessage) (string, error) {
					if env.Gate != nil {
						if ok, why := env.Gate.External(server + "." + t.Name); !ok {
							return "", fmt.Errorf("denied (%s)", why)
						}
					}
					env.mutate() // MCP tools may change files: checkpoint first
					// A server that hangs must not hang the turn.
					cctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
					defer cancel()
					c, err := conn.get(cctx)
					if err != nil {
						return "", err
					}
					res, isErr, err := c.CallTool(cctx, t.Name, args)
					if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
						return "", fmt.Errorf("the MCP server did not answer within %s", mcpCallTimeout)
					}
					if err != nil {
						if c.Err() != nil {
							// Not retried here: the call may have had effects.
							return "", fmt.Errorf("the MCP server %s stopped during this call (%v); it is started again on the next call%s", c.Name, err, c.LogTail())
						}
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

// mcpConn is one server's current connection, started again when it died.
type mcpConn struct {
	mu sync.Mutex
	c  *mcp.Client
}

func (m *mcpConn) get(ctx context.Context) (*mcp.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.c.Err() == nil {
		return m.c, nil
	}
	why := m.c.Err()
	nc, err := m.c.Restart(ctx)
	if err != nil {
		return nil, fmt.Errorf("the MCP server %s is down (%v) and could not be started again: %v%s", m.c.Name, why, err, m.c.LogTail())
	}
	m.c = nc
	return nc, nil
}
