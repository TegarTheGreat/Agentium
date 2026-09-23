package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// handle answers one JSON-RPC request like a tiny MCP server.
func handle(body []byte) (reply map[string]any, isRequest bool) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	json.Unmarshal(body, &req)
	if len(req.ID) == 0 {
		return nil, false
	}
	res := func(r any) map[string]any { return map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": r} }
	switch req.Method {
	case "initialize":
		return res(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}), true
	case "tools/list":
		return res(map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}), true
	case "tools/call":
		var p struct {
			Arguments map[string]string `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		return res(map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + p.Arguments["text"]}}}), true
	}
	return map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "nope"}}, true
}

func TestStreamableHTTP(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodDelete {
			mu.Lock()
			seen = append(seen, "DELETE "+r.Header.Get("Mcp-Session-Id"))
			mu.Unlock()
			return
		}
		body, _ := io.ReadAll(r.Body)
		reply, isReq := handle(body)
		var m struct{ Method string }
		json.Unmarshal(body, &m)
		mu.Lock()
		seen = append(seen, m.Method+" sid="+r.Header.Get("Mcp-Session-Id")+" v="+r.Header.Get("MCP-Protocol-Version"))
		mu.Unlock()
		if !isReq {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if m.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-1")
		}
		b, _ := json.Marshal(reply)
		if m.Method == "tools/list" { // reply as an SSE stream
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, ": comment\nevent: message\ndata: %s\n\n", b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	defer srv.Close()
	t.Setenv("MCP_TOKEN", "tok-123")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Start(ctx, "remote", Config{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer $MCP_TOKEN"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tools) != 1 || c.Tools[0].Name != "echo" {
		t.Fatalf("tools: %+v", c.Tools)
	}
	out, isErr, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || isErr || out != "echo: hi" {
		t.Fatalf("call: %q %v %v", out, isErr, err)
	}
	c.Close()
	mu.Lock()
	got := strings.Join(seen, "\n")
	mu.Unlock()
	for _, want := range []string{"initialize sid= v=", "notifications/initialized sid=sess-1 v=2025-06-18", "tools/list sid=sess-1", "tools/call sid=sess-1", "DELETE sess-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Wrong credentials give a clear error.
	os.Setenv("MCP_TOKEN", "wrong")
	if _, err := Start(ctx, "remote", Config{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer $MCP_TOKEN"}}, ""); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("auth error: %v", err)
	}
}

func TestLegacySSE(t *testing.T) {
	events := make(chan []byte, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /messages?sessionId=abc\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case b := <-events:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case r.Method == http.MethodPost && r.URL.Path == "/messages" && r.URL.Query().Get("sessionId") == "abc":
			body, _ := io.ReadAll(r.Body)
			if reply, ok := handle(body); ok {
				b, _ := json.Marshal(reply)
				events <- b
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Start(ctx, "legacy", Config{URL: srv.URL + "/sse", Type: "sse"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, _, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"yo"}`))
	if err != nil || out != "echo: yo" {
		t.Fatalf("call: %q %v", out, err)
	}
}

func TestStdioStderrInError(t *testing.T) {
	logp := t.TempDir() + "/mcp.log"
	_, err := Start(context.Background(), "broken", Config{Command: "sh", Args: []string{"-c", "echo 'missing API token' >&2; exit 1"}, LogPath: logp}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "missing API token") {
		t.Fatalf("error should show the server's stderr: %v", err)
	}
}
