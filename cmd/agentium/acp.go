package main

// agentium acp: an Agent Client Protocol (v1) server on stdin/stdout, so
// editors that speak ACP (Zed, JetBrains) can use Agentium as their
// agent. One JSON-RPC 2.0 message per line; stdout carries nothing else.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/lsp"
	"github.com/tegarthegreat/agentium/internal/mcp"
	"github.com/tegarthegreat/agentium/internal/memory"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/session"
	"github.com/tegarthegreat/agentium/internal/skill"
	"github.com/tegarthegreat/agentium/internal/tool"
)

const acpProtocol = 1

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpServer struct {
	out    io.Writer
	wmu    sync.Mutex
	nextID atomic.Int64
	mu     sync.Mutex
	calls  map[int64]chan rpcMsg // our requests to the client
	sess   map[string]*acpSession
	cfg    config.Config
	auth   config.Auth
	model  string
	log    func(string)
}

type acpSession struct {
	id       string
	a        *agent.Agent
	gate     *policy.Gate
	cwd      string
	mem      *memCtl
	clients  []*mcp.Client
	cleanup  []func()
	mu       sync.Mutex
	cancel   context.CancelFunc
	lastTool string
	allowed  map[string]bool // "allow always" answers, by action kind
	lastUsed time.Time       // for evicting idle sessions
}

// maxACPSessions bounds live sessions: each holds MCP servers, language
// servers and jobs, and editors open a new session per thread.
const maxACPSessions = 8

// close releases what the session holds.
func (ss *acpSession) close() {
	for _, f := range ss.cleanup {
		f()
	}
	for _, c := range ss.clients {
		c.Close()
	}
}

func cmdACP(args []string) error {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	modelRef := fs.String("m", "", "model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	s := &acpServer{out: os.Stdout, calls: map[int64]chan rpcMsg{}, sess: map[string]*acpSession{}, cfg: cfg, auth: auth,
		model: firstNonEmpty(*modelRef, os.Getenv("AGENTIUM_MODEL"), cfg.Model),
		log:   func(m string) { fmt.Fprintln(os.Stderr, "agentium acp:", m) }}
	defer s.closeAll()
	return s.serve(os.Stdin)
}

func (s *acpServer) serve(in io.Reader) error {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var m rpcMsg
			if json.Unmarshal(line, &m) != nil {
				s.write(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{-32700, "parse error"}})
			} else {
				s.handle(m)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *acpServer) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *acpServer) reply(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *acpServer) fail(id json.RawMessage, code int, msg string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{code, msg}})
}

func (s *acpServer) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request calls the client and waits for its answer.
func (s *acpServer) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)
	ch := make(chan rpcMsg, 1)
	s.mu.Lock()
	s.calls[id] = ch
	s.mu.Unlock()
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, errors.New(m.Error.Message)
		}
		return m.Result, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.calls, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (s *acpServer) handle(m rpcMsg) {
	if m.Method == "" { // a reply to one of our requests
		var id int64
		if json.Unmarshal(m.ID, &id) == nil {
			s.mu.Lock()
			ch := s.calls[id]
			delete(s.calls, id)
			s.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
		return
	}
	switch m.Method {
	case "initialize":
		s.reply(m.ID, map[string]any{
			"protocolVersion": acpProtocol,
			"agentCapabilities": map[string]any{
				"loadSession":        false,
				"promptCapabilities": map[string]any{"image": true, "audio": false, "embeddedContext": true},
				"mcpCapabilities":    map[string]any{"http": true, "sse": true},
			},
			"authMethods": []any{},
			"agentInfo":   map[string]any{"name": "agentium", "title": "Agentium", "version": version},
		})
	case "authenticate":
		s.reply(m.ID, map[string]any{})
	case "session/new":
		// Starting MCP servers can take seconds: don't hold up replies
		// (e.g. permission answers) for other sessions meanwhile.
		go s.newSession(m)
	case "session/set_mode":
		var p struct{ SessionID, ModeID string }
		json.Unmarshal(m.Params, &p)
		if mode, err := policy.CheckMode(p.ModeID); err != nil {
			s.fail(m.ID, -32602, err.Error())
		} else if ss := s.session(p.SessionID); ss != nil {
			ss.gate.SetMode(mode)
			s.reply(m.ID, map[string]any{})
			s.notify("session/update", map[string]any{"sessionId": ss.id, "update": map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": string(ss.gate.GetMode())}})
		} else {
			s.fail(m.ID, -32602, "unknown session")
		}
	case "session/prompt":
		go s.prompt(m)
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		json.Unmarshal(m.Params, &p)
		if ss := s.session(p.SessionID); ss != nil {
			ss.mu.Lock()
			if ss.cancel != nil {
				ss.cancel()
			}
			ss.mu.Unlock()
		}
	default:
		if len(m.ID) > 0 {
			s.fail(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

func (s *acpServer) session(id string) *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sess[id]; ss != nil {
		ss.lastUsed = time.Now()
		return ss
	}
	return nil
}

type acpNameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// acpMCPServer is an MCP server the client asks the session to use:
// stdio (no type), or {"type":"http"|"sse", url, headers}.
type acpMCPServer struct {
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Command string         `json:"command"`
	Args    []string       `json:"args"`
	Env     []acpNameValue `json:"env"`
	URL     string         `json:"url"`
	Headers []acpNameValue `json:"headers"`
}

func (s *acpServer) newSession(m rpcMsg) {
	var p struct {
		Cwd        string         `json:"cwd"`
		McpServers []acpMCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil || !filepath.IsAbs(p.Cwd) {
		s.fail(m.ID, -32602, "session/new needs an absolute cwd")
		return
	}
	cwd := p.Cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	res, err := provider.Resolve(s.model, s.cfg, s.auth)
	if err != nil {
		s.fail(m.ID, -32000, err.Error())
		return
	}
	ss := &acpSession{id: session.New(cwd, "").ID, cwd: cwd, allowed: map[string]bool{}}
	ss.gate = &policy.Gate{Mode: policy.ParseMode(s.cfg.Mode), Root: cwd, Protected: gitProtected(cwd)}
	ss.gate.SetRules(s.cfg.Permissions)
	ss.gate.Approve = func(action, reason string) bool { return s.askPermission(ss, action, reason) }

	tools := tool.All()
	servers := map[string]config.MCPServer{}
	for name, sc := range s.cfg.MCP {
		servers[name] = sc
	}
	for _, ms := range p.McpServers {
		sc := config.MCPServer{Command: ms.Command, Args: ms.Args, URL: ms.URL, Type: ms.Type, Literal: true}
		if ms.Type == "http" {
			sc.Type = ""
		}
		if len(ms.Env) > 0 {
			sc.Env = map[string]string{}
			for _, e := range ms.Env {
				sc.Env[e.Name] = e.Value
			}
		}
		if len(ms.Headers) > 0 {
			sc.Headers = map[string]string{}
			for _, h := range ms.Headers {
				sc.Headers[h.Name] = h.Value
			}
		}
		servers[ms.Name] = sc
	}
	if len(servers) > 0 {
		ss.clients = startMCP(servers, cwd, s.log).clients
		tools = append(tools, tool.MCPTools(ss.clients)...)
	}
	mem := openMemory(s.cfg, cwd)
	snapshot := ""
	if mem != nil {
		snapshot = mem.store.Snapshot()
		mem.skip = ss.id
		go mem.buildIndex()
	}
	skills := skill.Discover(config.Home(), cwd)
	a := &agent.Agent{
		Client: res.Client, Model: res.Model, System: agent.SystemPrompt(cwd, mem != nil, snapshot) + skill.Prompt(skills),
		Reasoning: res.Reasoning(s.cfg.Effort), FastMode: s.cfg.Fast, Tools: tools,
		Env:      &tool.Env{Root: cwd, Gate: ss.gate, AllowPrivateNet: s.cfg.FetchPrivate, Vision: res.Vision(), CodeCache: codeCache(cwd)},
		MaxTurns: firstNonZero(s.cfg.MaxTurns, -1), MaxTokens: s.cfg.MaxTokens, MaxOutput: res.Info.Output,
		ContextTokens: firstPositive(s.cfg.ContextTokens, res.Info.Context, provider.ContextWindow(res.Model)),
		Verify:        s.cfg.Verify == nil || *s.cfg.Verify,
	}
	a.Tools = append(a.Tools, a.TodoTool(), a.TaskTool())
	if s.cfg.OracleModel != "" {
		if o, err := oracleModel(s.cfg, s.auth); err == nil {
			a.Oracle = o
			a.Tools = append(a.Tools, a.OracleTool())
		} else {
			s.log("oracle_model ignored: " + firstLine(err.Error()))
		}
	}
	if s.cfg.SubagentModel != "" {
		if sub, _, err := subModel(s.cfg, s.auth, s.cfg.Effort); err == nil {
			a.Sub = sub
		} else {
			s.log("subagent_model ignored: " + firstLine(err.Error()))
		}
	}
	if mem != nil {
		a.Env.Recall = mem.search
		a.OnRemember = func(fact string) { mem.store.Apply([]memory.Directive{{Kind: "remember", Text: fact}}) }
	}
	if s.cfg.LSP == nil || *s.cfg.LSP {
		a.Env.LSP = lsp.NewManager(cwd, policy.ScrubEnv(os.Environ(), nil))
		ss.cleanup = append(ss.cleanup, a.Env.LSP.Close)
	}
	if s.cfg.Hooks != nil {
		a.Env.PostEdit = s.cfg.Hooks.PostEdit
		a.PreTool = preToolHook(s.cfg.Hooks.PreTool, cwd, s.log)
	}
	setupSandbox(a.Env, s.cfg, cwd, false)
	if store := openCheckpoints(s.cfg, cwd); store != nil {
		a.Env.BeforeMutate = func() { store.Snapshot(context.Background(), "before: acp prompt") }
	}
	if res.Known {
		info := res.Info
		a.Cost = func(us provider.Usage) float64 { return info.Price(us.Input, us.Output, us.CacheRead, us.CacheWrite) }
	}
	ss.a, ss.mem = a, mem
	ss.cleanup = append(ss.cleanup, a.Env.KillJobs)
	ss.lastUsed = time.Now()
	s.mu.Lock()
	s.sess[ss.id] = ss
	var evict []*acpSession
	for len(s.sess) > maxACPSessions {
		// Drop the least recently used idle session.
		var old *acpSession
		for _, c := range s.sess {
			c.mu.Lock()
			busy := c.cancel != nil
			c.mu.Unlock()
			if c != ss && !busy && (old == nil || c.lastUsed.Before(old.lastUsed)) {
				old = c
			}
		}
		if old == nil {
			break
		}
		delete(s.sess, old.id)
		evict = append(evict, old)
	}
	s.mu.Unlock()
	for _, old := range evict {
		old.close()
	}
	modes := []map[string]any{
		{"id": "auto", "name": "Auto", "description": "Ask only before risky actions"},
		{"id": "ask", "name": "Ask", "description": "Ask before every command and edit"},
		{"id": "plan", "name": "Plan", "description": "Read-only: investigate and propose a plan"},
		{"id": "yolo", "name": "Yolo", "description": "Never ask"},
	}
	s.reply(m.ID, map[string]any{"sessionId": ss.id, "modes": map[string]any{"currentModeId": string(ss.gate.GetMode()), "availableModes": modes}})
	var cmds []map[string]any
	for _, sk := range skills {
		cmds = append(cmds, map[string]any{"name": sk.Name, "description": sk.Description, "input": map[string]any{"hint": "task"}})
	}
	if len(cmds) > 0 {
		s.notify("session/update", map[string]any{"sessionId": ss.id, "update": map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": cmds}})
	}
}

// askPermission turns a policy question into session/request_permission.
func (s *acpServer) askPermission(ss *acpSession, action, reason string) bool {
	kind, _, _ := strings.Cut(action, ":")
	ss.mu.Lock()
	if ss.allowed[kind] {
		ss.mu.Unlock()
		return true
	}
	toolID := ss.lastTool
	ss.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if toolID == "" {
		toolID = "perm"
	}
	res, err := s.request(ctx, "session/request_permission", map[string]any{
		"sessionId": ss.id,
		"toolCall":  map[string]any{"toolCallId": toolID, "title": action + " (" + reason + ")"},
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
			{"optionId": "always", "name": "Always allow " + kind, "kind": "allow_always"},
			{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
		},
	})
	if err != nil {
		return false
	}
	var r struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	json.Unmarshal(res, &r)
	if r.Outcome.Outcome != "selected" {
		return false
	}
	switch r.Outcome.OptionID {
	case "allow":
		return true
	case "always":
		ss.mu.Lock()
		ss.allowed[kind] = true
		ss.mu.Unlock()
		return true
	}
	return false
}

var toolKinds = map[string]string{"read": "read", "edit": "edit", "bash": "execute", "search": "search", "fetch": "fetch", "todo": "think", "task": "other"}

func (s *acpServer) prompt(m rpcMsg) {
	var p struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	json.Unmarshal(m.Params, &p)
	ss := s.session(p.SessionID)
	if ss == nil {
		s.fail(m.ID, -32602, "unknown session")
		return
	}
	text, images := promptContent(p.Prompt)
	ctx, cancel := context.WithCancel(context.Background())
	ss.mu.Lock()
	if ss.cancel != nil {
		ss.mu.Unlock()
		cancel()
		s.fail(m.ID, -32600, "a prompt is already running in this session")
		return
	}
	ss.cancel = cancel
	ss.mu.Unlock()
	defer func() {
		ss.mu.Lock()
		ss.cancel = nil
		ss.mu.Unlock()
		cancel()
	}()
	a := ss.a
	update := func(u map[string]any) {
		s.notify("session/update", map[string]any{"sessionId": ss.id, "update": u})
	}
	var replies []string
	a.Events = agent.Events{
		Text: func(d string) {
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": d}})
		},
		ToolStart: func(c provider.ToolCall) {
			ss.mu.Lock()
			ss.lastTool = c.ID
			ss.mu.Unlock()
			u := map[string]any{"sessionUpdate": "tool_call", "toolCallId": c.ID, "title": summarizeCall(c),
				"kind": firstNonEmpty(toolKinds[c.Name], "other"), "status": "in_progress", "rawInput": c.Args}
			var args map[string]any
			if json.Unmarshal(c.Args, &args) == nil {
				if path, ok := args["path"].(string); ok && path != "" {
					if !filepath.IsAbs(path) {
						path = filepath.Join(ss.cwd, path)
					}
					u["locations"] = []map[string]any{{"path": path}}
				}
			}
			update(u)
		},
		ToolDone: func(c provider.ToolCall, out string, err error, d time.Duration) {
			status := "completed"
			if err != nil {
				status = "failed"
				out = strings.TrimSpace(out + "\n" + err.Error())
			}
			update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": c.ID, "status": status,
				"content": []map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": clipText(out, 4000)}}}})
			if c.Name == "todo" {
				var entries []map[string]any
				for _, t := range a.Ledger.Todos() {
					st := map[string]string{"done": "completed", "in_progress": "in_progress"}[t.Status]
					entries = append(entries, map[string]any{"content": t.Text, "priority": "medium", "status": firstNonEmpty(st, "pending")})
				}
				update(map[string]any{"sessionUpdate": "plan", "entries": entries})
			}
		},
		Notice: func(msg string) {
			update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "· " + msg + "\n"}})
		},
		TurnFinish: func(r provider.Response) {
			if r.Text != "" {
				replies = append(replies, r.Text)
			}
		},
	}
	send := text
	a.Attach = images
	if msg, ok, err := skill.Invoke(skill.Discover(config.Home(), ss.cwd), text); ok && err == nil {
		send = msg
	}
	if ss.mem != nil {
		if block, n := ss.mem.recall(text); n > 0 {
			send = block + "\n\n" + send
		}
	}
	_, err := a.Run(ctx, send)
	if ss.mem != nil {
		ss.mem.afterTurn(text, replies, a.Ledger.TurnEdited(), a.Ledger.TurnErrors(), a.Ledger.Lessons(), a.Ledger.Untrusted(), func(string) {})
	}
	stop := "end_turn"
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		stop = "cancelled"
	case errors.Is(err, agent.ErrMaxTurns), errors.Is(err, agent.ErrStuck), errors.Is(err, agent.ErrBudget):
		stop = "max_turn_requests"
	case errors.Is(err, agent.ErrTruncated):
		stop = "max_tokens"
	case errors.Is(err, agent.ErrRefused):
		stop = "refusal"
	case err != nil:
		s.fail(m.ID, -32603, err.Error())
		return
	}
	s.reply(m.ID, map[string]any{"stopReason": stop})
}

// promptContent turns ACP content blocks into text and images.
func promptContent(blocks []json.RawMessage) (string, []provider.Image) {
	var sb strings.Builder
	var images []provider.Image
	for _, raw := range blocks {
		var b struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
			URI      string `json:"uri"`
			Name     string `json:"name"`
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "image":
			if data, err := base64.StdEncoding.DecodeString(b.Data); err == nil && len(data) <= tool.MaxImageBytes {
				images = append(images, provider.Image{MediaType: b.MimeType, Data: data})
			}
		case "resource_link":
			fmt.Fprintf(&sb, "\n[referenced: %s]", strings.TrimPrefix(b.URI, "file://"))
		case "resource":
			if b.Resource.Text != "" {
				fmt.Fprintf(&sb, "\n<file path=%q>\n%s\n</file>", strings.TrimPrefix(b.Resource.URI, "file://"), b.Resource.Text)
			}
		}
	}
	return strings.TrimSpace(sb.String()), images
}

func (s *acpServer) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ss := range s.sess {
		ss.close()
	}
}

// firstNonZero is a, or b when a is 0 (an IDE session has no step limit
// unless "max_turns" sets one).
func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
