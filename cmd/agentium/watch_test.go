package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchAIComments(t *testing.T) {
	dir := t.TempDir()
	tick := time.Now()
	write := func(name, text string) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
		tick = tick.Add(time.Second) // each save is newer than the last
		os.Chtimes(p, tick, tick)
	}
	write("a.go", "package a\n// old note AI!\n")
	files := func() []string { return []string{"a.go", "b.py"} }
	w := &watcher{root: dir, list: files, mtimes: map[string]time.Time{}, seen: map[string]bool{}}
	w.scan(true)
	if w.take() != "" {
		t.Fatal("comments already there at start are not sent")
	}
	write("b.py", "x = 1  # the parser AI\ndef f():\n    pass  # handle empty input here AI!\n# why is this slow? AI?\n")
	w.scan(false)
	msg := w.take()
	for _, want := range []string{`b.py:1 (AI): "the parser"`, `b.py:3 (AI!): "handle empty input here"`, `b.py:4 (AI?): "why is this slow?"`, "answer the AI? questions"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in:\n%s", want, msg)
		}
	}
	w.scan(false)
	if w.take() != "" {
		t.Fatal("sent once only")
	}
	if n := aiNotes(filepath.Join(dir, "a.go"), "a.go"); len(n) != 1 || n[0].mark != "AI!" || n[0].text != "old note" {
		t.Fatalf("notes %+v", n)
	}
	// A leading mark works too.
	write("b.py", "x = 2  # AI! add a docstring\n")
	w.scan(false)
	if m := w.take(); !strings.Contains(m, `(AI!): "add a docstring"`) {
		t.Fatalf("leading mark: %q", m)
	}
	// Handled before it was sent: nothing to send.
	write("b.py", "y = 3  # rename this AI!\n")
	w.scan(false)
	write("b.py", "y = 3\n")
	if m := w.take(); m != "" {
		t.Fatalf("stale: %q", m)
	}
	// Prose files and vendored code are not the user's instructions.
	for _, f := range []string{"README.md", "vendor/x.go"} {
		if !skipWatch(f) {
			t.Fatalf("%s is watched", f)
		}
	}
	// Words that merely contain AI are not markers.
	write("b.py", "# SAID it, MAIN loop\n# plain comment\n")
	w.scan(false)
	if m := w.take(); m != "" {
		t.Fatalf("false positive: %q", m)
	}
}
