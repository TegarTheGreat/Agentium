package skill

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverParseAndInvoke(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	sub := filepath.Join(repo, "pkg")
	write(t, filepath.Join(home, "skills", "review", "SKILL.md"), "---\nname: review\ndescription: user review\n---\nuser body")
	write(t, filepath.Join(repo, ".agentium", "skills", "review", "SKILL.md"), "---\nname: review\ndescription: >\n  Review a diff\n  carefully.\n---\n\n# Review\nCheck tests.\n")
	write(t, filepath.Join(sub, ".claude", "skills", "Deploy", "SKILL.md"), "no frontmatter, name from dir")
	write(t, filepath.Join(repo, ".agentium", "skills", "bad name", "SKILL.md"), "---\nname: Bad Name!\n---\n")

	got := Discover(home, sub)
	if len(got) != 2 || got[0].Name != "deploy" || got[1].Name != "review" {
		t.Fatalf("discover: %+v", got)
	}
	if got[1].Source != "project" || got[1].Description != "Review a diff carefully." {
		t.Fatalf("project skill must override the user one and parse folded text: %+v", got[1])
	}
	p := Prompt(got)
	if !strings.Contains(p, "- review: Review a diff carefully.") || Prompt(nil) != "" {
		t.Fatalf("prompt: %q", p)
	}
	msg, ok, err := Invoke(got, "/review the auth change")
	if !ok || err != nil || !strings.Contains(msg, "# Review\nCheck tests.") || !strings.HasSuffix(msg, "Task: the auth change") || strings.Contains(msg, "description:") {
		t.Fatalf("invoke: %q %v %v", msg, ok, err)
	}
	if _, ok, _ := Invoke(got, "/nope"); ok {
		t.Fatal("unknown skill must not match")
	}
}

func TestInstallFromGitPinnedAndRemove(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "skills", "lint", "SKILL.md"), "---\nname: lint\ndescription: run the linter\n---\nRun scripts/lint.sh")
	write(t, filepath.Join(src, "skills", "lint", "scripts", "lint.sh"), "#!/bin/sh\necho lint\n")
	write(t, filepath.Join(src, "README.md"), "not a skill")
	os.Symlink("/etc/passwd", filepath.Join(src, "skills", "lint", "leak"))
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.email=a@b", "-c", "user.name=a", "commit", "-qm", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	st, err := Stage("file://" + src)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if len(st.Commit) != 40 {
		t.Fatalf("commit not pinned: %q", st.Commit)
	}
	found := st.Found()
	if len(found) != 1 || found[0].Name != "lint" {
		t.Fatalf("found: %+v", found)
	}
	if sc := Scripts(found[0].Dir()); len(sc) != 1 || sc[0] != filepath.Join("scripts", "lint.sh") {
		t.Fatalf("scripts: %v", sc)
	}
	dst, err := Install(home, st, found[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "leak")); err == nil {
		t.Fatal("symlink copied")
	}
	if _, err := Install(home, st, found[0]); err != ErrExists {
		t.Fatalf("second install: %v", err)
	}
	sk, _ := Parse(filepath.Join(dst, "SKILL.md"))
	if o := Origin(sk); !strings.HasSuffix(o, "@"+st.Commit) {
		t.Fatalf("origin: %q", o)
	}
	if err := Remove(home, "lint"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(home, "../x"); err == nil {
		t.Fatal("remove must reject path names")
	}
}

func TestShadowed(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	write(t, filepath.Join(home, "skills", "deploy", "SKILL.md"), "---\nname: deploy\n---\nmine")
	write(t, filepath.Join(repo, ".agentium", "skills", "deploy", "SKILL.md"), "---\nname: deploy\n---\ntheirs")
	write(t, filepath.Join(repo, ".agentium", "skills", "lint", "SKILL.md"), "---\nname: lint\n---\nx")
	sh := Shadowed(home, repo)
	if len(sh) != 1 || !strings.Contains(sh[0], "/deploy") {
		t.Fatalf("shadowed: %v", sh)
	}
}
