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
	write := func(name, text string) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(text), 0o644)
		now := time.Now().Add(time.Duration(len(text)) * time.Millisecond)
		os.Chtimes(p, now, now)
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
	for _, want := range []string{"b.py:1: the parser (AI)", "b.py:3: handle empty input here (AI!)", "b.py:4: why is this slow? (AI?)", "answer the AI? questions"} {
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
	// Words that merely contain AI are not markers.
	write("b.py", "# SAID it, MAIN loop\n# plain comment\n")
	w.scan(false)
	if m := w.take(); m != "" {
		t.Fatalf("false positive: %q", m)
	}
}
