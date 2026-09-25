package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/provider"
)

func TestHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh hooks")
	}
	dir := t.TempDir()
	var warned []string
	warn := func(s string) { warned = append(warned, s) }
	pre := preToolHook([]config.ToolHook{
		{Match: "bash", Command: `grep -q 'rm -rf' && { echo "no deleting here" >&2; exit 2; }; exit 0`},
		{Match: "edit|write", Command: "exit 1"}, // broken: reported, not blocking
		{Match: "(", Command: "true"},
	}, dir, warn)
	call := func(name, args string) error {
		return pre(context.Background(), provider.ToolCall{Name: name, Args: []byte(args)})
	}
	if err := call("bash", `{"cmd":"rm -rf build"}`); !errors.Is(err, errHookStop) || !strings.Contains(err.Error(), "no deleting here") {
		t.Fatalf("rm: %v", err)
	}
	if err := call("bash", `{"cmd":"ls"}`); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if err := call("edit", `{}`); err != nil || len(warned) != 2 { // bad regex + failing hook
		t.Fatalf("edit: %v %q", err, warned)
	}
	if err := call("read", `{}`); err != nil {
		t.Fatal(err)
	}

	added, err := promptHooks([]string{`echo "branch: main"`, `grep -q secret && exit 2; true`}, dir, "hello", warn)
	if err != nil || added != "branch: main" {
		t.Fatalf("prompt hooks: %q %v", added, err)
	}
	if _, err := promptHooks([]string{`grep -q secret && exit 2; true`}, dir, "the secret is x", warn); !errors.Is(err, errHookStop) {
		t.Fatalf("expected a stop, got %v", err)
	}
	if got := sessionHooks([]string{"echo started"}, dir, warn); got != "started" {
		t.Fatalf("session: %q", got)
	}
}

func TestUserWords(t *testing.T) {
	s := "<session-start>\nx\n</session-start>\n\n<hook-context>\ny\n</hook-context>\n\n<recall n=\"1\">z</recall>\n\nfix the bug"
	if got := provider.UserWords(s); got != "fix the bug" {
		t.Fatalf("got %q", got)
	}
	if got := provider.UserWords("plain <recall> text"); got != "plain <recall> text" {
		t.Fatalf("got %q", got)
	}
}

func TestHookTimeoutEndsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh hooks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	runHook(ctx, "sleep 5 & sleep 5; true", t.TempDir(), nil)
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("hook ran %v past its deadline", d)
	}
}
