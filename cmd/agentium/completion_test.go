package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileSuggestionsLargeProject(t *testing.T) {
	c := &completer{listed: time.Now()}
	for i := 0; i < 60000; i++ {
		c.files = append(c.files, fmt.Sprintf("pkg/m%d/file%d.go", i%50, i))
	}
	c.files = append(c.files, "internal/zebra/handler.go") // past the old 20k cap
	got := c.fileSuggestions("zebhand")
	if len(got) == 0 || got[0].label != "handler.go" {
		t.Fatalf("got %v", got)
	}
	got = c.fileSuggestions("file1")
	if len(got) != 50 {
		t.Fatalf("got %d suggestions, want 50", len(got))
	}
	for i := 1; i < len(got); i++ {
		a, _ := fuzzyScore(strings.TrimSpace(got[i-1].insert[1:]), "file1")
		b, _ := fuzzyScore(strings.TrimSpace(got[i].insert[1:]), "file1")
		if a < b {
			t.Fatalf("not ranked: %q before %q", got[i-1].insert, got[i].insert)
		}
	}
}

func TestStagedAmong(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Skip(string(out))
		}
	}
	run("init", "-q")
	os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	os.WriteFile(filepath.Join(repo, "sub", "a.go"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(repo, "sub", "b.go"), []byte("b"), 0o644)
	run("add", ".")
	run("commit", "-qm", "x")
	run("mv", "sub/a.go", "sub/a2.go")
	got := stagedAmong(filepath.Join(repo, "sub"), []string{"a.go", "a2.go", "b.go"})
	if strings.Join(got, ",") != "a.go,a2.go" {
		t.Fatalf("got %v", got)
	}
}
