package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktree(t *testing.T) {
	t.Setenv("AGENTIUM_HOME", t.TempDir())
	repo := t.TempDir()
	run := func(dir string, args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(repo, "init", "-q")
	os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	os.WriteFile(filepath.Join(repo, "sub", "a.txt"), []byte("a"), 0o644)
	run(repo, "add", ".")
	run(repo, "commit", "-qm", "init", "--no-gpg-sign")

	if _, _, err := enterWorktree(repo, "../bad"); err == nil {
		t.Fatal("a name with a path must be refused")
	}
	dir, done, err := enterWorktree(filepath.Join(repo, "sub"), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "sub" || !strings.Contains(dir, "worktrees") {
		t.Fatalf("dir %s", dir)
	}
	if got := run(dir, "branch", "--show-current"); got != "agentium/w1" {
		t.Fatalf("branch %q", got)
	}
	if gitCommonDir(dir) == "" || gitCommonDir(repo) != "" {
		t.Fatal("common dir: the worktree's is outside it, the main one's is not")
	}
	if msg := done(); msg != "" {
		t.Fatalf("an untouched worktree is removed, got %q", msg)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("worktree still there")
	}
	dir, done, _ = enterWorktree(repo, "w2")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	if msg := done(); !strings.Contains(msg, "agentium/w2") {
		t.Fatalf("a changed worktree is kept, got %q", msg)
	}
}
