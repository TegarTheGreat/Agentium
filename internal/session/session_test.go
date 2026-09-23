package session

import (
	"path/filepath"
	"testing"

	"github.com/tegarthegreat/agentium/internal/provider"
)

func TestSpecialCharsInCwd(t *testing.T) {
	t.Setenv("AGENTIUM_HOME", t.TempDir())
	cwd := filepath.Join("/src", "R&D <team>")
	s := New(cwd, "m")
	s.Messages = []provider.Message{{Role: provider.RoleUser, Text: "hi"}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Latest(cwd)
	if err != nil || got == nil || got.ID != s.ID {
		t.Fatalf("session for %q not found: %v %v", cwd, got, err)
	}
}
