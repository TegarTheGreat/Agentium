package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a fake MCP server when AGENTIUM_FAKE_MCP is set.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTIUM_FAKE_MCP") != "" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(sc.Bytes(), &req)
		if len(req.ID) == 0 {
			continue // notification
		}
		reply := func(result any) { out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
		switch req.Method {
		case "initialize":
			// A server-initiated ping first, which the client must answer.
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "ping"})
			reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}})
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			json.Unmarshal(req.Params, &p)
			if p.Cursor == "" {
				reply(map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo text", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}, "nextCursor": "p2"})
			} else {
				reply(map[string]any{"tools": []any{map[string]any{"name": "fail", "description": "Always fails", "inputSchema": map[string]any{"type": "object"}}}})
			}
		case "tools/call":
			var p struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			if p.Name == "crash" {
				os.Exit(3)
			}
			if p.Name == "fail" {
				reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "boom"}}, "isError": true})
			} else {
				reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + p.Arguments["text"]}, map[string]any{"type": "image", "mimeType": "image/png", "data": "xx"}}})
			}
		default:
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "nope"}})
		}
	}
}

func TestClient(t *testing.T) {
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Start(ctx, "fake", Config{Command: self, Env: map[string]string{"AGENTIUM_FAKE_MCP": "1"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools) != 2 || c.Tools[0].Name != "echo" || c.Tools[1].Name != "fail" {
		t.Fatalf("tools = %+v (pagination)", c.Tools)
	}
	out, isErr, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || isErr || !strings.HasPrefix(out, "echo: hi") || !strings.Contains(out, "image content omitted") {
		t.Fatalf("call: %q %v %v", out, isErr, err)
	}
	if out, isErr, _ := c.CallTool(ctx, "fail", nil); !isErr || out != "boom" {
		t.Fatalf("fail: %q %v", out, isErr)
	}
	// Parallel calls are matched to their responses.
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			out, _, err := c.CallTool(ctx, "echo", json.RawMessage(fmt.Sprintf(`{"text":"n%d"}`, i)))
			if err == nil && !strings.HasPrefix(out, fmt.Sprintf("echo: n%d", i)) {
				err = fmt.Errorf("mismatched reply %q for %d", out, i)
			}
			errs <- err
		}(i)
	}
	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if ToolName("gh", "list_issues") != "mcp__gh__list_issues" {
		t.Fatal(ToolName("gh", "list_issues"))
	}
	a, b := ToolName("my server", "do.thing"), ToolName("my server", "do_thing")
	if a == b || !strings.HasPrefix(a, "mcp__my_server__do_thing_") {
		t.Fatalf("sanitized names must not collide: %s %s", a, b)
	}
	long := strings.Repeat("x", 80)
	if l1, l2 := ToolName("s", long+"1"), ToolName("s", long+"2"); l1 == l2 || len(l1) > 64 {
		t.Fatalf("long names: %s %s", l1, l2)
	}
}

func TestCappedLog(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	l := &cappedLog{f: f, left: 10}
	for i := 0; i < 5; i++ {
		if n, err := l.Write([]byte("0123456")); n != 7 || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	f.Close()
	b, _ := os.ReadFile(f.Name())
	if !strings.HasPrefix(string(b), "0123456012\n[log truncated") {
		t.Fatalf("log = %q", b)
	}
}

func TestRestartAfterCrash(t *testing.T) {
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, "fake", Config{Command: self, Env: map[string]string{"AGENTIUM_FAKE_MCP": "1"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cur := c
	for i := 0; i < MaxRestarts; i++ {
		if _, _, err := cur.CallTool(ctx, "crash", nil); err == nil {
			t.Fatal("a crashing call succeeded")
		}
		for deadline := time.Now().Add(5 * time.Second); cur.Err() == nil && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		if cur.Err() == nil {
			t.Fatal("a dead server still looks up")
		}
		nc, err := cur.Restart(ctx)
		if err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
		if out, _, err := nc.CallTool(ctx, "echo", json.RawMessage(`{"text":"back"}`)); err != nil || !strings.HasPrefix(out, "echo: back") {
			t.Fatalf("after restart: %q %v", out, err)
		}
		cur = nc
	}
	cur.CallTool(ctx, "crash", nil)
	for deadline := time.Now().Add(5 * time.Second); cur.Err() == nil && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := cur.Restart(ctx); err == nil || !strings.Contains(err.Error(), "not restarting") {
		t.Fatalf("restarts must stop after %d: %v", MaxRestarts, err)
	}
}
