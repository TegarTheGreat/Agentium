package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestCorruptReportedAndOwn(t *testing.T) {
	t.Setenv("AGENTIUM_HOME", t.TempDir())
	s := New("/proj", "m")
	s.Messages = []provider.Message{{Role: provider.RoleUser, Text: "hi"}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir(), "99999999-bad.json")
	os.WriteFile(bad, []byte(`{"id":"x","cwd":"/proj","messages":[{"role`), 0o600)
	got, _ := Latest("/proj")
	if got == nil || got.ID != s.ID || len(TakeCorrupt()) != 1 {
		t.Fatal("the damaged newest file must be reported, the valid one found")
	}
	release, ok := s.Own()
	if !ok {
		t.Fatal("first owner")
	}
	defer release()
}

func TestUnreadableSessionNotLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTIUM_HOME", home)
	dir := filepath.Join(home, "sessions")
	os.MkdirAll(dir, 0o700)
	// An older or foreign format: messages carry "content", not "text".
	foreign := `{"id":"20240101-000000.000","cwd":"/w","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`
	os.WriteFile(filepath.Join(dir, "20240101-000000.000.json"), []byte(foreign), 0o600)
	newer := `{"version":99,"id":"20240102-000000.000","cwd":"/w","messages":[{"role":"user","text":"hi"}]}`
	os.WriteFile(filepath.Join(dir, "20240102-000000.000.json"), []byte(newer), 0o600)
	TakeCorrupt()
	if s, _ := Latest("/w"); s != nil {
		t.Fatalf("loaded %s", s.ID)
	}
	got := strings.Join(TakeCorrupt(), "\n")
	if !strings.Contains(got, "newer agentium") || !strings.Contains(got, "no content this version can read") {
		t.Fatalf("reported: %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "20240101-000000.000.json")); string(b) != foreign {
		t.Fatal("the file was changed")
	}
}

func TestPruneRemovesStaleLocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTIUM_HOME", home)
	s := New("/w", "m")
	s.Messages = []provider.Message{{Role: provider.RoleUser, Text: "hi"}}
	s.Save()
	release, _ := s.Own()
	dir := filepath.Join(home, "sessions")
	os.WriteFile(filepath.Join(dir, "gone.lock"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "held.lock"), nil, 0o600)
	held, _ := (&Session{ID: "held"}).Own()
	defer held()
	Prune(100, time.Hour)
	if _, err := os.Stat(filepath.Join(dir, "gone.lock")); err == nil {
		t.Error("a lock without a session was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "held.lock")); err != nil {
		t.Error("a lock another process holds was removed")
	}
	if _, err := os.Stat(filepath.Join(dir, s.ID+".lock")); err != nil {
		t.Error("a live session's lock was removed")
	}
	release()
}
