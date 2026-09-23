package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	if !strings.Contains(out, "== b.py\n1: class Svc") || !strings.Contains(out, "== pkg/a.go") || strings.Contains(out, "notes.md") || strings.Contains(out, "def start") {
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
