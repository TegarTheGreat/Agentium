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
}

func dir() string { return filepath.Join(config.Home(), "sessions") }

// New starts a session id for cwd.
func New(cwd, model string) *Session {
	return &Session{ID: time.Now().Format("20060102-150405.000"), Cwd: cwd, Model: model}
}

// Save writes the session atomically.
func (s *Session) Save() error {
	if len(s.Messages) == 0 {
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
