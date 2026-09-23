// Package tool implements Agentium's five tools: read, edit, bash, search,
// fetch. Schemas are kept tiny on purpose: every byte is sent every turn.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/sandbox"
)

// Env is shared by all tools in a session.
type Env struct {
	Root string
	Gate *policy.Gate
	// Vision: the model accepts images, so read attaches image files.
	Vision bool
	// AllowPrivateNet lets fetch reach localhost and private networks.
	AllowPrivateNet bool
	// Sandbox confines bash commands; nil runs them unconfined.
	Sandbox *sandbox.Config
	// Net decides whether a sandboxed command may use the network.
	Net policy.NetPolicy
	// PostEdit are shell commands run after each successful edit, with
	// {path} replaced by the edited file (e.g. "gofmt -w {path}").
	PostEdit []string
	// BeforeMutate, if set, runs once per turn before the first edit or
	// bash call. It is used to checkpoint the workspace for undo.
	BeforeMutate func()

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	mutOnce *sync.Once
	seen    map[string]stamp
}

// stamp identifies a file version the model has seen.
type stamp struct {
	size int64
	mod  int64
}

func statStamp(p string) (stamp, bool) {
	st, err := os.Stat(p)
	if err != nil {
		return stamp{}, false
	}
	return stamp{st.Size(), st.ModTime().UnixNano()}, true
}

// StartTurn re-arms BeforeMutate for a new user turn.
func (e *Env) StartTurn() {
	e.mu.Lock()
	e.mutOnce = &sync.Once{}
	e.mu.Unlock()
}

func (e *Env) mutate() {
	if e.BeforeMutate == nil {
		return
	}
	e.mu.Lock()
	if e.mutOnce == nil {
		e.mutOnce = &sync.Once{}
	}
	once := e.mutOnce
	e.mu.Unlock()
	once.Do(e.BeforeMutate)
}

// markSeen records the current version of p as known to the model.
func (e *Env) markSeen(p string) {
	st, ok := statStamp(p)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen == nil {
		e.seen = map[string]stamp{}
	}
	if ok {
		e.seen[p] = st
	} else {
		delete(e.seen, p)
	}
}

// freshness reports whether the model has seen p, and whether p changed
// on disk since.
func (e *Env) freshness(p string) (seen, stale bool) {
	e.mu.Lock()
	old, ok := e.seen[p]
	e.mu.Unlock()
	if !ok {
		return false, false
	}
	cur, exists := statStamp(p)
	return true, !exists || cur != old
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

// real resolves symlinks in p (or, for a path that does not exist yet,
// in its nearest existing parent), so policy checks see where a write or
// read actually lands: a symlink in the workspace must not be a door out.
func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, rest := filepath.Dir(p), filepath.Base(p)
	for i := 0; i < 64 && dir != filepath.Dir(dir); i++ {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = filepath.Dir(dir)
	}
	return p
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
	return fmt.Sprintf("%s\n[... %d bytes omitted; narrow the output (grep, head, tail, sed -n) to see them ...]\n%s", h, cut, t)
}
