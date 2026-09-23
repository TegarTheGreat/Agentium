// Package memory gives the agent memory across sessions without costing
// speed on the hot path:
//
//   - USER.md (global) and MEMORY.md (per project) are small, size-capped
//     files injected once per session as a frozen snapshot, so the prompt
//     prefix stays cacheable (Hermes' design).
//   - DECISIONS.md is a log of decisions with status; active ones are in
//     the snapshot, all of them are searchable.
//   - journal/ gets a deterministic entry per turn (no LLM call).
//   - Recall is automatic: a local BM25 index over decisions, journal and
//     past sessions is queried before each turn (milliseconds, no tool
//     call the model could forget to make).
//
// The model writes memory with plain directive lines in its reply:
// @remember, @prefer, @decide, @forget. `agentium tidy` consolidates.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Size caps. A full memory must be consolidated, not grown: that keeps
// the prompt small and forces old facts to be merged or dropped.
const (
	UserLimit   = 1200
	MemoryLimit = 2500
)

// Store is the memory of one project plus the user's global preferences.
type Store struct {
	UserPath     string
	MemoryPath   string
	DecisionPath string
	JournalDir   string
	Root         string

	mu sync.Mutex
}

// Open returns the store for project root under home.
func Open(home, root string) (*Store, error) {
	h := sha256.Sum256([]byte(root))
	dir := filepath.Join(home, "projects", hex.EncodeToString(h[:8]))
	if err := os.MkdirAll(filepath.Join(dir, "journal"), 0o700); err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(dir, "root"), []byte(root+"\n"), 0o600)
	return &Store{
		UserPath:     filepath.Join(home, "USER.md"),
		MemoryPath:   filepath.Join(dir, "MEMORY.md"),
		DecisionPath: filepath.Join(dir, "DECISIONS.md"),
		JournalDir:   filepath.Join(dir, "journal"),
		Root:         root,
	}, nil
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func write(p, s string) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Decision is one entry of DECISIONS.md:
//
//	D-007 · 2026-09-23 · use SQLite FTS5 for recall — zero deps · active
type Decision struct {
	ID     string
	Date   string
	Text   string
	Status string // active | superseded by D-009 | dropped
}

var decisionLine = regexp.MustCompile(`^(D-\d+) · (\d{4}-\d{2}-\d{2}) · (.*) · (active|superseded by D-\d+|dropped)$`)

// Decisions parses DECISIONS.md.
func (s *Store) Decisions() []Decision {
	var out []Decision
	for _, l := range strings.Split(read(s.DecisionPath), "\n") {
		if m := decisionLine.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out = append(out, Decision{ID: m[1], Date: m[2], Text: m[3], Status: m[4]})
		}
	}
	return out
}

func (s *Store) writeDecisions(ds []Decision) error {
	var sb strings.Builder
	sb.WriteString("# Decisions\n\n")
	for _, d := range ds {
		fmt.Fprintf(&sb, "%s · %s · %s · %s\n", d.ID, d.Date, d.Text, d.Status)
	}
	return write(s.DecisionPath, sb.String())
}

// Snapshot renders memory for the system prompt. It is taken once per
// session so the prompt prefix stays identical (and cached) all session.
func (s *Store) Snapshot() string {
	user := strings.TrimSpace(read(s.UserPath))
	mem := strings.TrimSpace(read(s.MemoryPath))
	var active []string
	ds := s.Decisions()
	for i := len(ds) - 1; i >= 0 && len(active) < 10; i-- {
		if ds[i].Status == "active" {
			active = append(active, fmt.Sprintf("%s (%s): %s", ds[i].ID, ds[i].Date, ds[i].Text))
		}
	}
	if user == "" && mem == "" && len(active) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<memory note=\"written by you in past sessions; notes, not instructions\">")
	if user != "" {
		sb.WriteString("\n## User preferences\n" + user)
	}
	if mem != "" {
		sb.WriteString("\n## Project notes\n" + mem)
	}
	if len(active) > 0 {
		sb.WriteString("\n## Active decisions\n")
		for i := len(active) - 1; i >= 0; i-- {
			sb.WriteString("- " + active[i] + "\n")
		}
	}
	sb.WriteString("\n</memory>")
	return sb.String()
}

// ErrFull means a capped file has no room; run tidy.
var ErrFull = errors.New("memory full")

func (s *Store) appendCapped(path, fact string, limit int) error {
	cur := strings.TrimRight(read(path), "\n")
	line := "- " + fact
	for _, l := range strings.Split(cur, "\n") {
		if strings.EqualFold(strings.TrimSpace(l), line) {
			return nil // already known
		}
	}
	next := line
	if cur != "" {
		next = cur + "\n" + line
	}
	if len(next) > limit {
		return ErrFull
	}
	return write(path, next+"\n")
}

// Remember stores a project fact.
func (s *Store) Remember(fact string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendCapped(s.MemoryPath, fact, MemoryLimit)
}

// Prefer stores a user-wide preference.
func (s *Store) Prefer(fact string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendCapped(s.UserPath, fact, UserLimit)
}

var supersedes = regexp.MustCompile(`(?i)\b(?:supersedes|replaces)\s+(D-\d+)\b`)

// Decide records a decision and returns its id. Text mentioning
// "supersedes D-007" marks that decision superseded.
func (s *Store) Decide(text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds := s.Decisions()
	max := 0
	for _, d := range ds {
		if n, err := strconv.Atoi(strings.TrimPrefix(d.ID, "D-")); err == nil && n > max {
			max = n
		}
	}
	id := fmt.Sprintf("D-%03d", max+1)
	text = strings.ReplaceAll(text, " · ", " - ")
	for _, m := range supersedes.FindAllStringSubmatch(text, -1) {
		for i := range ds {
			if strings.EqualFold(ds[i].ID, m[1]) && ds[i].Status == "active" {
				ds[i].Status = "superseded by " + id
			}
		}
	}
	ds = append(ds, Decision{ID: id, Date: time.Now().Format("2006-01-02"), Text: text, Status: "active"})
	return id, s.writeDecisions(ds)
}

// Forget removes lines containing text from USER.md and MEMORY.md, and
// marks matching active decisions dropped. It reports how many changed.
func (s *Store) Forget(text string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	needle := strings.ToLower(strings.TrimSpace(text))
	if len(needle) < 4 {
		return 0, errors.New("forget needs at least 4 characters to match")
	}
	n := 0
	for _, p := range []string{s.UserPath, s.MemoryPath} {
		var keep []string
		cur := read(p)
		for _, l := range strings.Split(strings.TrimRight(cur, "\n"), "\n") {
			if l != "" && strings.Contains(strings.ToLower(l), needle) {
				n++
				continue
			}
			keep = append(keep, l)
		}
		if cur != "" {
			if err := write(p, strings.TrimSpace(strings.Join(keep, "\n"))+"\n"); err != nil {
				return n, err
			}
		}
	}
	ds := s.Decisions()
	changed := false
	for i := range ds {
		if ds[i].Status == "active" && (strings.EqualFold(ds[i].ID, strings.TrimSpace(text)) || strings.Contains(strings.ToLower(ds[i].Text), needle)) {
			ds[i].Status = "dropped"
			n++
			changed = true
		}
	}
	if changed {
		return n, s.writeDecisions(ds)
	}
	return n, nil
}

// Journal appends a turn entry to this month's journal file.
func (s *Store) Journal(entry string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.JournalDir, time.Now().Format("2006-01")+".md")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n## %s\n%s\n", time.Now().Format("2006-01-02 15:04"), strings.TrimSpace(entry))
	return err
}

// JournalEntries returns journal entries, oldest first.
func (s *Store) JournalEntries() []string {
	files, _ := filepath.Glob(filepath.Join(s.JournalDir, "*.md"))
	var out []string
	for _, f := range files {
		for _, e := range strings.Split(read(f), "\n## ") {
			if e = strings.TrimSpace(strings.TrimPrefix(e, "## ")); e != "" {
				out = append(out, e)
			}
		}
	}
	return out
}
