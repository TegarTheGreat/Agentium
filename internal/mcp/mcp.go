// Package mcp is a minimal Model Context Protocol client over stdio:
// enough to start a server, list its tools and call them. MCP tools are
// only loaded when configured, so they cost nothing otherwise.
package mcp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tegarthegreat/agentium/internal/policy"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes one server (config.json "mcp" entries): a local
// command (stdio), or a remote URL (Streamable HTTP, or legacy SSE with
// Type "sse").
type Config struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Type    string            `json:"type,omitempty"` // "", "stdio", "http", "sse"
	Headers map[string]string `json:"headers,omitempty"`
	// LogPath receives a stdio server's stderr ("" = discard).
	LogPath string `json:"-"`
	// Tokens holds OAuth grants for remote servers (nil = no OAuth).
	Tokens *TokenStore `json:"-"`
	// Literal: Env and Headers are used as given, without $VAR expansion
	// (values from an editor over ACP, which may contain '$').
	Literal bool `json:"-"`
}

// Tool is a tool offered by a server.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// transport moves JSON-RPC messages; replies reach Client.dispatch.
type transport interface {
	send(ctx context.Context, msg []byte) error
	close()
}

// Client is a running server connection.
type Client struct {
	Name  string
	Tools []Tool

	tr     transport
	nextID atomic.Int64
	mu     sync.Mutex
	wait   map[int64]chan response
	closed chan struct{}
	once   sync.Once
	err    error

	cfg      Config
	dir      string
	restarts int     // how many times Restart replaced this server
	next     *Client // the replacement after a Restart, closed with this one
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	response
}

const protocolVersion = "2025-06-18"

// Start connects to the server, performs the handshake and lists its tools.
func Start(ctx context.Context, name string, cfg Config, dir string) (*Client, error) {
	c := &Client{Name: name, wait: map[int64]chan response{}, closed: make(chan struct{}), cfg: cfg, dir: dir}
	switch {
	case cfg.URL != "" && cfg.Type == "sse":
		tr, err := startSSE(ctx, c, cfg)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", name, err)
		}
		c.tr = tr
	case cfg.URL != "":
		c.tr = &httpTransport{c: c, url: cfg.URL, headers: cfg.expand(cfg.Headers), tokens: cfg.Tokens}
	case cfg.Command != "":
		tr, err := startStdio(c, cfg, dir)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", name, err)
		}
		c.tr = tr
	default:
		return nil, fmt.Errorf("mcp %s: needs a command or a url", name)
	}

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentium", "version": "0.10"},
	}, &init); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w%s", name, err, logTail(cfg.LogPath))
	}
	if h, ok := c.tr.(*httpTransport); ok && init.ProtocolVersion != "" {
		h.mu.Lock()
		h.proto = init.ProtocolVersion
		h.mu.Unlock()
	}
	_ = c.notify("notifications/initialized", nil)
	cursor := ""
	for page := 0; page < 20; page++ {
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			c.Close()
			return nil, fmt.Errorf("mcp %s: tools/list: %w", name, err)
		}
		c.Tools = append(c.Tools, res.Tools...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	return c, nil
}

func (cfg Config) expand(h map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[k] = cfg.value(v)
	}
	return out
}

func (cfg Config) value(v string) string {
	if cfg.Literal {
		return v
	}
	return os.ExpandEnv(v)
}

// logTail returns the last lines of a server's stderr log, to explain a
// failed start.
func logTail(p string) string {
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return "\n  server stderr (" + p + "):\n  " + strings.Join(lines, "\n  ")
}

// dispatch handles one message from the server.
func (c *Client) dispatch(raw []byte) {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		// Server-to-client request (ping, roots/list, sampling ...).
		if m.Method == "ping" {
			c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
		} else {
			c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "not supported"}})
		}
	case m.Method != "":
		// Notification: ignored.
	default:
		var id int64
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		c.mu.Lock()
		ch := c.wait[id]
		delete(c.wait, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- m.response
		}
	}
}

// fail ends one pending call with an error (its reply will not come).
func (c *Client) fail(id int64, why string) {
	c.mu.Lock()
	ch := c.wait[id]
	delete(c.wait, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- response{Error: &rpcError{Code: -32000, Message: why}}
	}
}

// shutdown marks the connection dead and fails pending calls.
func (c *Client) shutdown(why string) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = errors.New(why)
		for id, ch := range c.wait {
			close(ch)
			delete(c.wait, id)
		}
		c.mu.Unlock()
		close(c.closed)
	})
}

// Err reports why the connection is dead, or nil while it is up.
func (c *Client) Err() error {
	select {
	case <-c.closed:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err == nil {
			return errors.New("closed")
		}
		return c.err
	default:
		return nil
	}
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return c.tr.send(ctx, b)
}

// --- stdio ---

// maxLogBytes caps a stdio server's stderr log, so a chatty server
// cannot fill the disk over a long session.
const maxLogBytes = 2 << 20

// cappedLog writes until its budget is spent, then drops the rest.
type cappedLog struct {
	f    *os.File
	left int
}

func (l *cappedLog) Write(p []byte) (int, error) {
	if l.left <= 0 {
		return len(p), nil
	}
	b := p
	if len(b) > l.left {
		b = b[:l.left]
	}
	l.left -= len(b)
	l.f.Write(b)
	if l.left <= 0 {
		l.f.WriteString("\n[log truncated: more than 2 MiB]\n")
	}
	return len(p), nil
}

type stdioTransport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex
	log   *os.File
}

func startStdio(c *Client, cfg Config, dir string) (*stdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = dir
	// A server gets the secrets named in its own config, not every key of
	// the session.
	cmd.Env = policy.ScrubEnv(os.Environ(), nil)
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+cfg.value(v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin}
	cmd.Stderr = io.Discard
	if cfg.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err == nil {
			if f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
				t.log = f
				cmd.Stderr = &cappedLog{f: f, left: maxLogBytes}
				// The log is copied through a pipe: a grandchild that keeps
				// it open (npx, uvx, wrapper scripts) must not hang Close.
				cmd.WaitDelay = time.Second
			}
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		r := bufio.NewReaderSize(stdout, 64*1024)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				c.dispatch(line)
			}
			if err != nil {
				break
			}
		}
		c.shutdown("server exited")
	}()
	return t, nil
}

func (t *stdioTransport) send(_ context.Context, b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_, err := t.stdin.Write(append(b, '\n'))
	return err
}

func (t *stdioTransport) close() {
	_ = t.stdin.Close()
	done := make(chan struct{})
	go func() { t.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		<-done
	}
	if t.log != nil {
		t.log.Close()
	}
}

func (c *Client) notify(method string, params any) error {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return c.send(m)
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.wait[id] = ch
	c.mu.Unlock()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := c.tr.send(ctx, b); err != nil {
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return errors.New("server exited")
		}
		if r.Error != nil {
			return fmt.Errorf("%s (code %d)", r.Error.Message, r.Error.Code)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		_ = c.notify("notifications/cancelled", map[string]any{"requestId": id})
		return ctx.Err()
	}
}

// CallTool runs a tool and renders its result as text.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var res struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			MimeType string          `json:"mimeType"`
			Resource json.RawMessage `json:"resource"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return "", false, err
	}
	var sb strings.Builder
	for _, p := range res.Content {
		switch p.Type {
		case "text":
			sb.WriteString(p.Text + "\n")
		case "resource":
			sb.WriteString(string(p.Resource) + "\n")
		default:
			fmt.Fprintf(&sb, "[%s content omitted %s]\n", p.Type, p.MimeType)
		}
	}
	if sb.Len() == 0 && len(res.StructuredContent) > 0 {
		sb.Write(res.StructuredContent)
	}
	return strings.TrimSpace(sb.String()), res.IsError, nil
}

// Close disconnects from (or stops) the server, and any replacement
// Restart started.
func (c *Client) Close() {
	c.tr.close()
	c.shutdown("closed")
	c.mu.Lock()
	next := c.next
	c.mu.Unlock()
	if next != nil {
		next.Close()
	}
}

// MaxRestarts bounds how often a crashed server is started again in one
// session: one that keeps crashing is broken, not unlucky.
const MaxRestarts = 3

// Restart starts a server whose connection died again and returns the
// new connection (Close on c closes it too).
func (c *Client) Restart(ctx context.Context) (*Client, error) {
	c.mu.Lock()
	n := c.restarts
	c.mu.Unlock()
	if n >= MaxRestarts {
		return nil, fmt.Errorf("mcp %s: stopped %d times, not restarting again this session", c.Name, n)
	}
	nc, err := Start(ctx, c.Name, c.cfg, c.dir)
	c.mu.Lock()
	c.restarts++
	if err == nil {
		nc.restarts = c.restarts
		c.next = nc
	}
	c.mu.Unlock()
	return nc, err
}

// LogTail is the end of the server's stderr log, to explain a failure
// ("" when there is none).
func (c *Client) LogTail() string { return logTail(c.cfg.LogPath) }

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ToolName is the model-facing name: mcp__<server>__<tool>, at most 64
// chars. Sanitizing or shortening a name adds a short hash of the
// original so distinct tools never collide.
func ToolName(server, tool string) string {
	raw := "mcp__" + server + "__" + tool
	n := "mcp__" + unsafeName.ReplaceAllString(server, "_") + "__" + unsafeName.ReplaceAllString(tool, "_")
	if n == raw && len(n) <= 64 {
		return n
	}
	h := sha256.Sum256([]byte(raw))
	suffix := "_" + hex.EncodeToString(h[:3])
	if len(n)+len(suffix) > 64 {
		n = n[:64-len(suffix)]
	}
	return n + suffix
}
