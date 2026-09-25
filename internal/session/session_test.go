package session

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

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

func TestPruneKeepsNamedSessions(t *testing.T) {
	t.Setenv("AGENTIUM_HOME", t.TempDir())
	var ids []string
	for i := 0; i < 3; i++ {
		s := New("/p", "m")
		s.ID = fmt.Sprintf("20260101-00000%d.000", i)
		s.Messages = []provider.Message{{Role: provider.RoleUser, Text: fmt.Sprint("ask ", i)}}
		if i == 0 {
			s.Title = "keep me"
		}
		if i == 1 { // a tool call's "title" is not the session's
			s.Messages = append(s.Messages, provider.Message{Role: provider.RoleAssistant,
				ToolCalls: []provider.ToolCall{{ID: "1", Name: "mcp__gh__create_issue", Args: []byte(`{"title":"bug"}`)}}})
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	Prune(1, time.Hour)
	list, _ := ForCwd("/p", 10)
	var got []string
	for _, s := range list {
		got = append(got, s.Label())
	}
	if len(ids) != 3 || len(list) != 2 || list[1].Title != "keep me" {
		t.Fatalf("after prune: %q", got)
	}
}
