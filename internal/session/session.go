// Package session saves conversations so they can be continued with -c.
package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/fsx"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// Session is one saved conversation.
type Session struct {
	// Version is the file format (FormatVersion when written).
	Version  int                `json:"version,omitempty"`
	ID       string             `json:"id"`
	Cwd      string             `json:"cwd"`
	Title    string             `json:"title,omitempty"` // set with /rename
	Model    string             `json:"model"`
	Updated  time.Time          `json:"updated"`
	Messages []provider.Message `json:"messages"`
	// Checkpoints are workspace snapshots taken before each turn that
	// changed something, newest last.
	Checkpoints []Checkpoint `json:"checkpoints,omitempty"`
	// Note is delivered to the model with the next input (see agent.Note).
	Note string `json:"note,omitempty"`
	// SystemHash identifies the system prompt the messages were produced
	// under; signed thinking blocks are only valid under the same one.
	SystemHash string `json:"system_hash,omitempty"`
}

// Checkpoint is a restorable workspace snapshot.
type Checkpoint struct {
	ID     string    `json:"id"`
	Prompt string    `json:"prompt"`
	Time   time.Time `json:"time"`
	// After is the snapshot taken when the turn ended: undo reverts only
	// what changed between ID and After.
	After string `json:"after,omitempty"`
}

const maxCheckpoints = 50

// FormatVersion is the session file format this build writes. A file
// with a higher one is left alone rather than misread and overwritten.
const FormatVersion = 1

// AddCheckpoint appends a snapshot, keeping the newest maxCheckpoints.
func (s *Session) AddCheckpoint(id, prompt string) {
	s.Checkpoints = append(s.Checkpoints, Checkpoint{ID: id, Prompt: prompt, Time: time.Now()})
	if n := len(s.Checkpoints); n > maxCheckpoints {
		s.Checkpoints = append([]Checkpoint(nil), s.Checkpoints[n-maxCheckpoints:]...)
	}
}

// EndCheckpoint records the end-of-turn snapshot of the newest checkpoint.
func (s *Session) EndCheckpoint(after string) {
	if n := len(s.Checkpoints); n > 0 && s.Checkpoints[n-1].After == "" {
		s.Checkpoints[n-1].After = after
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
	s.Version = FormatVersion
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// A unique temporary file: two processes never write the same one.
	return fsx.WriteFile(filepath.Join(dir(), s.ID+".json"), b, 0o600)
}

// Own marks this process as the one writing the session; ok is false when
// another running agentium already has it open (then continue in a copy,
// or one of the two would overwrite the other's turns).
func (s *Session) Own() (release func(), ok bool) {
	return fsx.TryLock(filepath.Join(dir(), s.ID+".lock"))
}

var (
	corruptMu sync.Mutex
	corrupt   []string
)

// TakeCorrupt returns (once) the session files for the folder that could
// not be read in the last listing.
func TakeCorrupt() []string {
	corruptMu.Lock()
	defer corruptMu.Unlock()
	c := corrupt
	corrupt = nil
	return c
}

// Latest returns the most recent session for cwd, or nil.
func Latest(cwd string) (*Session, error) {
	ss, err := ForCwd(cwd, 1)
	if err != nil || len(ss) == 0 {
		return nil, err
	}
	return ss[0], nil
}

// ForCwd returns up to max sessions for cwd, newest first.
func ForCwd(cwd string, max int) ([]*Session, error) {
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
	// Same encoding as the saved file (json escapes &, <, >).
	quotedCwd, _ := json.Marshal(cwd)
	var out []*Session
	for _, n := range names {
		// Read only the head first: cwd is near the start, and sessions
		// with screenshots can be megabytes.
		f, err := os.Open(filepath.Join(dir(), n))
		if err != nil {
			continue
		}
		head := make([]byte, 4096)
		k, _ := io.ReadFull(f, head)
		if !bytes.Contains(head[:k], quotedCwd) {
			f.Close()
			continue
		}
		rest, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			continue
		}
		b := append(head[:k], rest...)
		var s Session
		if err := json.Unmarshal(b, &s); err != nil {
			// A damaged file (a crash while writing, a full disk): say so
			// rather than quietly continue an older conversation.
			corruptMu.Lock()
			corrupt = append(corrupt, filepath.Join(dir(), n)+": "+err.Error())
			corruptMu.Unlock()
			continue
		}
		if why := unreadable(&s); why != "" && s.Cwd == cwd {
			// Loading it would send blank messages and, on save, replace
			// the file with them.
			corruptMu.Lock()
			corrupt = append(corrupt, filepath.Join(dir(), n)+": "+why)
			corruptMu.Unlock()
			continue
		}
		if s.Cwd == cwd {
			out = append(out, &s)
			if len(out) >= max {
				break
			}
		}
	}
	return out, nil
}

// unreadable says why a decoded session cannot be used, or "".
func unreadable(s *Session) string {
	if s.Version > FormatVersion {
		return fmt.Sprintf("written by a newer agentium (format %d); update to continue it", s.Version)
	}
	empty := 0
	for _, m := range s.Messages {
		if m.Role == "" || m.Text == "" && len(m.ToolCalls) == 0 && len(m.Images) == 0 && len(m.Raw) == 0 && m.ToolCallID == "" && m.Reasoning == "" {
			empty++
		}
	}
	if len(s.Messages) > 0 && empty*2 > len(s.Messages) {
		return fmt.Sprintf("%d of %d messages have no content this version can read", empty, len(s.Messages))
	}
	return ""
}

// Prune deletes sessions beyond the newest keep, and any older than
// maxAge, so heavy use cannot fill the disk.
func Prune(keep int, maxAge time.Duration) {
	ents, err := os.ReadDir(dir())
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, n := range names {
		p := filepath.Join(dir(), n)
		old := i >= keep
		if !old {
			st, err := os.Stat(p)
			old = err == nil && time.Since(st.ModTime()) > maxAge
		}
		if old && !titled(p) {
			_ = os.Remove(p) // a session the user named is kept
		}
	}
	// Lock files of sessions that are gone, unless a process holds one.
	for _, e := range ents {
		n := e.Name()
		if filepath.Ext(n) != ".lock" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir(), strings.TrimSuffix(n, ".lock")+".json")); err == nil {
			continue
		}
		if release, ok := fsx.TryLock(filepath.Join(dir(), n)); ok {
			_ = os.Remove(filepath.Join(dir(), n))
			release()
		}
	}
}

// titled reports whether the saved session at p has a name: its
// top-level "title", read without loading the messages.
func titled(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return false
		}
		key, _ := t.(string)
		if key == "messages" {
			return false // title comes before the messages
		}
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return false
		}
		if key == "title" {
			var title string
			return json.Unmarshal(v, &title) == nil && title != ""
		}
	}
	return false
}

// Label is the session's title, or its first request.
func (s *Session) Label() string {
	if s.Title != "" {
		return s.Title
	}
	for _, m := range s.Messages {
		if m.Role == provider.RoleUser && m.Text != "" {
			return strings.Join(strings.Fields(provider.UserWords(m.Text)), " ")
		}
	}
	return "(empty)"
}
