package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tegarthegreat/agentium/internal/policy"
)

func env(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	return &Env{Root: dir, Gate: &policy.Gate{Mode: policy.Auto, Root: dir}}
}

func call(t *testing.T, tl Tool, e *Env, args string) (string, error) {
	t.Helper()
	return tl.Run(context.Background(), e, json.RawMessage(args))
}

func write(t *testing.T, e *Env, name, content string) {
	t.Helper()
	p := filepath.Join(e.Root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRead(t *testing.T) {
	e := env(t)
	write(t, e, "a.txt", "one\ntwo\nthree\nfour\n")
	out, err := call(t, readTool, e, `{"path":"a.txt"}`)
	if err != nil || out != "one\ntwo\nthree\nfour\n" {
		t.Fatalf("%q %v", out, err)
	}
	out, _ = call(t, readTool, e, `{"path":"a.txt","offset":2,"limit":2}`)
	if !strings.HasPrefix(out, "two\nthree\n") || !strings.Contains(out, "lines 2-3 of 4") {
		t.Fatalf("slice = %q", out)
	}
	out, _ = call(t, readTool, e, `{"path":"."}`)
	if !strings.Contains(out, "a.txt") {
		t.Fatalf("dir listing = %q", out)
	}
	write(t, e, "bin", "a\x00b")
	out, _ = call(t, readTool, e, `{"path":"bin"}`)
	if !strings.Contains(out, "binary") {
		t.Fatalf("binary = %q", out)
	}
	if _, err := call(t, readTool, e, `{"path":"missing"}`); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestEdit(t *testing.T) {
	e := env(t)
	if _, err := call(t, editTool, e, `{"path":"d/new.txt","new":"hello\nworld\n"}`); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(e.Root, "d/new.txt"))
	if string(b) != "hello\nworld\n" {
		t.Fatalf("created = %q", b)
	}
	if _, err := call(t, editTool, e, `{"path":"d/new.txt","old":"world","new":"there"}`); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(e.Root, "d/new.txt"))
	if string(b) != "hello\nthere\n" {
		t.Fatalf("edited = %q", b)
	}
	write(t, e, "dup.txt", "x x x")
	if _, err := call(t, editTool, e, `{"path":"dup.txt","old":"x","new":"y"}`); err == nil || !strings.Contains(err.Error(), "3 times") {
		t.Fatalf("ambiguous edit: %v", err)
	}
	if out, err := call(t, editTool, e, `{"path":"dup.txt","old":"x","new":"y","all":true}`); err != nil || !strings.Contains(out, "3 replaced") {
		t.Fatalf("all: %q %v", out, err)
	}
	if _, err := call(t, editTool, e, `{"path":"dup.txt","old":"nope","new":"y"}`); err == nil {
		t.Fatal("want not-found error")
	}
	// Writing outside the workspace needs approval; there is no approver.
	if _, err := call(t, editTool, e, `{"path":"../escape.txt","new":"x"}`); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("outside write: %v", err)
	}
}

func TestEditParallelSameFile(t *testing.T) {
	e := env(t)
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, "line%02d\n", i)
	}
	write(t, e, "f.txt", sb.String())
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			_, err := call(t, editTool, e, fmt.Sprintf(`{"path":"f.txt","old":"line%02d","new":"LINE%02d"}`, i, i))
			done <- err
		}(i)
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(e.Root, "f.txt"))
	if strings.Contains(string(b), "line") {
		t.Fatalf("lost updates:\n%s", b)
	}
}

func TestBash(t *testing.T) {
	e := env(t)
	out, err := call(t, bashTool, e, `{"cmd":"echo hi; echo err >&2; pwd"}`)
	if err != nil || !strings.Contains(out, "hi") || !strings.Contains(out, "err") || !strings.Contains(out, e.Root) {
		t.Fatalf("%q %v", out, err)
	}
	out, err = call(t, bashTool, e, `{"cmd":"echo nope; exit 3"}`)
	if err != nil || !strings.Contains(out, "[exit 3]") {
		t.Fatalf("exit code: %q %v", out, err)
	}
	t0 := time.Now()
	_, err = call(t, bashTool, e, `{"cmd":"sleep 5","timeout":1}`)
	if err == nil || time.Since(t0) > 4*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(t0))
	}
	out, _ = call(t, bashTool, e, `{"cmd":"seq 1 100000"}`)
	if len(out) > bashMaxOutput+200 || !strings.Contains(out, "omitted") || !strings.HasSuffix(strings.TrimSpace(out), "100000") {
		t.Fatalf("clip: len=%d tail=%q", len(out), out[len(out)-20:])
	}
	// Background child holding the pipe must not hang the tool.
	t0 = time.Now()
	if _, err := call(t, bashTool, e, `{"cmd":"sleep 30 & echo started"}`); err != nil || time.Since(t0) > 5*time.Second {
		t.Fatalf("background: %v after %s", err, time.Since(t0))
	}
	if _, err := call(t, bashTool, e, `{"cmd":"rm -rf /tmp/whatever"}`); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("risky command should be denied without approver: %v", err)
	}
}

func TestSearch(t *testing.T) {
	e := env(t)
	write(t, e, "src/main.go", "package main\nfunc Hello() {}\n")
	write(t, e, "src/util.go", "package main\nfunc helper() {}\n")
	write(t, e, "README.md", "hello docs\n")
	write(t, e, ".github/ci.yml", "run: func Hello\n")
	write(t, e, ".git/config", "func Hello in git internals\n")
	check := func(name string) {
		out, _ := call(t, searchTool, e, `{"pattern":"run: func"}`)
		if !strings.Contains(out, ".github/ci.yml") {
			t.Fatalf("%s: hidden files must be searched: %q", name, out)
		}
		if out, _ := call(t, searchTool, e, `{"pattern":"git internals"}`); out != "(no matches)" {
			t.Fatalf("%s: .git must be skipped: %q", name, out)
		}
		out, err := call(t, searchTool, e, `{"pattern":"func [Hh]el"}`)
		if err != nil || !strings.Contains(out, "src/main.go:2:") || !strings.Contains(out, "src/util.go:2:") {
			t.Fatalf("%s grep: %q %v", name, out, err)
		}
		out, _ = call(t, searchTool, e, `{"glob":"*.go"}`)
		if !strings.Contains(out, "src/main.go") || strings.Contains(out, "README") {
			t.Fatalf("%s glob: %q", name, out)
		}
		out, _ = call(t, searchTool, e, `{"pattern":"HELLO","ignore_case":true,"glob":"*.md"}`)
		if !strings.Contains(out, "README.md:1:") {
			t.Fatalf("%s icase: %q", name, out)
		}
		out, _ = call(t, searchTool, e, `{"pattern":"zzz_nothing"}`)
		if out != "(no matches)" {
			t.Fatalf("%s none: %q", name, out)
		}
	}
	check("default")
	// Force the pure-Go fallback.
	out, err := walkSearch(context.Background(), e.Root, e.Root, "func [Hh]el", "", false)
	if err != nil || !strings.Contains(out, "src/main.go:2:") || !strings.Contains(out, ".github/ci.yml") || strings.Contains(out, "internals") {
		t.Fatalf("fallback: %q %v", out, err)
	}
	out, _ = walkSearch(context.Background(), e.Root, e.Root, "", "src/**/*.go", false)
	if !strings.Contains(out, "src/util.go") {
		t.Fatalf("fallback glob: %q", out)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>x</title><script>evil()</script></head><body><h1>Title</h1><p>Hello &amp; welcome</p><ul><li>one</li></ul></body></html>`)
	}))
	defer srv.Close()
	// httptest listens on 127.0.0.1, which is blocked by default.
	if _, err := call(t, fetchTool, env(t), fmt.Sprintf(`{"url":%q}`, srv.URL)); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("loopback fetch must be blocked: %v", err)
	}
	for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/", "http://[::1]:9/", "http://localhost:1/"} {
		if _, err := call(t, fetchTool, env(t), fmt.Sprintf(`{"url":%q}`, u)); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Fatalf("%s must be blocked: %v", u, err)
		}
	}
	e := env(t)
	e.AllowPrivateNet = true
	out, err := call(t, fetchTool, e, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil || !strings.Contains(out, "# Title") || !strings.Contains(out, "Hello & welcome") || strings.Contains(out, "evil") || !strings.Contains(out, "untrusted") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := call(t, fetchTool, env(t), `{"url":"file:///etc/passwd"}`); err == nil {
		t.Fatal("non-http scheme must fail")
	}
}

func TestClip(t *testing.T) {
	s := strings.Repeat("a", 100)
	if Clip(s, 200) != s {
		t.Fatal("short strings unchanged")
	}
	c := Clip(strings.Repeat("x\n", 5000), 1000)
	if len(c) > 1100 || !strings.Contains(c, "omitted") {
		t.Fatalf("clip len %d", len(c))
	}
}

func TestSchemasAreValidJSON(t *testing.T) {
	for _, tl := range All() {
		if !json.Valid(tl.Def.Schema) {
			t.Fatalf("%s schema invalid", tl.Def.Name)
		}
	}
}

func TestEditGuards(t *testing.T) {
	e := env(t)
	write(t, e, "a.go", "package a\n")
	// Whole-file overwrite of an unread existing file is refused.
	if _, err := call(t, editTool, e, `{"path":"a.go","new":"oops"}`); err == nil || !strings.Contains(err.Error(), "read it first") {
		t.Fatalf("blind overwrite: %v", err)
	}
	call(t, readTool, e, `{"path":"a.go"}`)
	if _, err := call(t, editTool, e, `{"path":"a.go","new":"package a\n\nvar X = 1\n"}`); err != nil {
		t.Fatalf("overwrite after read: %v", err)
	}
	// Changed behind the agent's back: must re-read.
	time.Sleep(10 * time.Millisecond)
	write(t, e, "a.go", "package a\n\nvar X = 2 // edited by user\n")
	if _, err := call(t, editTool, e, `{"path":"a.go","old":"var X","new":"var Y"}`); err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("stale edit: %v", err)
	}
	call(t, readTool, e, `{"path":"a.go"}`)
	if _, err := call(t, editTool, e, `{"path":"a.go","old":"var X","new":"var Y"}`); err != nil {
		t.Fatalf("edit after re-read: %v", err)
	}
	// Consecutive own edits don't trip the stale check.
	if _, err := call(t, editTool, e, `{"path":"a.go","old":"var Y","new":"var Z"}`); err != nil {
		t.Fatalf("second own edit: %v", err)
	}
}

func TestBeforeMutateOncePerTurn(t *testing.T) {
	e := env(t)
	var n int32
	var mu sync.Mutex
	e.BeforeMutate = func() { mu.Lock(); n++; mu.Unlock(); time.Sleep(50 * time.Millisecond) }
	e.StartTurn()
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func(i int) {
			call(t, editTool, e, fmt.Sprintf(`{"path":"f%d.txt","new":"x"}`, i))
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	call(t, readTool, e, `{"path":"f0.txt"}`) // reads never checkpoint
	if n != 1 {
		t.Fatalf("BeforeMutate ran %d times in one turn", n)
	}
	e.StartTurn()
	call(t, bashTool, e, `{"cmd":"true"}`)
	if n != 2 {
		t.Fatalf("new turn should checkpoint again, got %d", n)
	}
}
