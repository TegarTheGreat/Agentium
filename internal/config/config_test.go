package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetKeepsOtherFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTIUM_HOME", home)
	path := filepath.Join(Home(), "config.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"mode":"ask","custom":{"x":1}}`), 0o600)
	if err := Set("model", "deepseek/deepseek-flash"); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || c.Model != "deepseek/deepseek-flash" || c.Mode != "ask" {
		t.Fatalf("%+v %v", c, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"custom"`) {
		t.Fatalf("unknown field lost: %s", b)
	}
}

func TestRepoRootIgnoresHomeRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".git"), 0o755) // dotfiles in git
	proj := filepath.Join(home, "code", "newproj")
	os.MkdirAll(proj, 0o755)
	if got := ProjectRoot(proj); got != proj {
		t.Fatalf("a repository at home must not swallow %s: got %s", proj, got)
	}
	if got := ProjectRoot(home); got != home {
		t.Fatalf("home itself: got %s", got)
	}
	repo := filepath.Join(home, "code", "app")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.MkdirAll(filepath.Join(repo, "src"), 0o755)
	if got := ProjectRoot(filepath.Join(repo, "src")); got != repo {
		t.Fatalf("a real repository: got %s", got)
	}
}
