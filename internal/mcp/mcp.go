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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes one stdio server (config.json "mcp" entries).
type Config struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Tool is a tool offered by a server.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client is a running server connection.
type Client struct {
	Name  string
	Tools []Tool

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	nextID atomic.Int64
	mu     sync.Mutex
	wait   map[int64]chan response
	closed chan struct{}
	err    error
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	response
}

const protocolVersion = "2025-06-18"

// Start launches the server, performs the handshake and lists its tools.
func Start(ctx context.Context, name string, cfg Config, dir string) (*Client, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("mcp %s: no command", name)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = dir
	// A server gets the secrets named in its own config, not every key of
	// the session.
	cmd.Env = policy.ScrubEnv(os.Environ(), nil)
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: %w", name, err)
	}
	c := &Client{Name: name, cmd: cmd, stdin: stdin, wait: map[int64]chan response{}, closed: make(chan struct{})}
	go c.read(stdout)

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentium", "version": "0.6"},
	}, &init); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", name, err)
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

func (c *Client) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
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
				continue
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
	c.mu.Lock()
	c.err = errors.New("server exited")
	for id, ch := range c.wait {
		close(ch)
		delete(c.wait, id)
	}
	c.mu.Unlock()
	close(c.closed)
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
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
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
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

// Close stops the server.
func (c *Client) Close() {
	_ = c.stdin.Close()
	select {
	case <-c.closed:
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
	_ = c.cmd.Wait()
}

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
