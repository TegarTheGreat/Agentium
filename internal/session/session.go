// Package session saves conversations so they can be continued with -c.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// Session is one saved conversation.
type Session struct {
	ID       string             `json:"id"`
	Cwd      string             `json:"cwd"`
	Model    string             `json:"model"`
	Updated  time.Time          `json:"updated"`
	Messages []provider.Message `json:"messages"`
	// Checkpoints are workspace snapshots taken before each turn that
	// changed something, newest last.
	Checkpoints []Checkpoint `json:"checkpoints,omitempty"`
	// Note is delivered to the model with the next input (see agent.Note).
	Note string `json:"note,omitempty"`
}

// Checkpoint is a restorable workspace snapshot.
type Checkpoint struct {
	ID     string    `json:"id"`
	Prompt string    `json:"prompt"`
	Time   time.Time `json:"time"`
}

const maxCheckpoints = 50

// AddCheckpoint appends a snapshot, keeping the newest maxCheckpoints.
func (s *Session) AddCheckpoint(id, prompt string) {
	s.Checkpoints = append(s.Checkpoints, Checkpoint{ID: id, Prompt: prompt, Time: time.Now()})
	if n := len(s.Checkpoints); n > maxCheckpoints {
		s.Checkpoints = append([]Checkpoint(nil), s.Checkpoints[n-maxCheckpoints:]...)
	}
}

// PopCheckpoint removes and returns the newest snapshot.
func (s *Session) PopCheckpoint() (Checkpoint, bool) {
	n := len(s.Checkpoints)
	if n == 0 {
		return Checkpoint{}, false
	}
	cp := s.Checkpoints[n-1]
	s.Checkpoints = s.Checkpoints[:n-1]
	return cp, true
}

func dir() string { return filepath.Join(config.Home(), "sessions") }

// New starts a session id for cwd.
func New(cwd, model string) *Session {
	return &Session{ID: time.Now().Format("20060102-150405.000"), Cwd: cwd, Model: model}
}

// Save writes the session atomically.
func (s *Session) Save() error {
	if len(s.Messages) == 0 && len(s.Checkpoints) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return err
	}
	s.Updated = time.Now()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	p := filepath.Join(dir(), s.ID+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Latest returns the most recent session for cwd, or nil.
func Latest(cwd string) (*Session, error) {
	ents, err := os.ReadDir(dir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	// IDs are timestamps, so name order is time order.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir(), n))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(b, &s) == nil && s.Cwd == cwd {
			return &s, nil
		}
	}
	return nil, nil
}
