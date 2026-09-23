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
