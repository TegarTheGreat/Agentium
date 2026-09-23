// Package tool implements Agentium's five tools: read, edit, bash, search,
// fetch. Schemas are kept tiny on purpose: every byte is sent every turn.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// Env is shared by all tools in a session.
type Env struct {
	Root string
	Gate *policy.Gate

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock serializes writes to the same file when tools run in parallel.
func (e *Env) lock(path string) func() {
	e.mu.Lock()
	if e.locks == nil {
		e.locks = map[string]*sync.Mutex{}
	}
	m := e.locks[path]
	if m == nil {
		m = &sync.Mutex{}
		e.locks[path] = m
	}
	e.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// abs resolves p against the workspace root.
func (e *Env) abs(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := userHome(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Root, p)
	}
	return filepath.Clean(p)
}

// Tool is one callable tool.
type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, env *Env, args json.RawMessage) (string, error)
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// All returns the tool set in a stable order (stable = cacheable prefix).
func All() []Tool {
	return []Tool{readTool, editTool, bashTool, searchTool, fetchTool}
}

// Defs returns the tool definitions for the model.
func Defs(ts []Tool) []provider.ToolDef {
	d := make([]provider.ToolDef, len(ts))
	for i, t := range ts {
		d[i] = t.Def
	}
	return d
}

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// Clip keeps the head and tail of s when it exceeds max bytes.
func Clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max / 4
	tail := max - head
	cut := len(s) - head - tail
	h, t := s[:head], s[len(s)-tail:]
	// Avoid splitting lines in the middle where cheap.
	if i := strings.LastIndexByte(h, '\n'); i > head/2 {
		h = h[:i+1]
	}
	if i := strings.IndexByte(t, '\n'); i >= 0 && i < tail/2 {
		t = t[i+1:]
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", h, cut, t)
}
