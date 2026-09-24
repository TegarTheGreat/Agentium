package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/tegarthegreat/agentium/internal/fsx"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/memory"
	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/session"
)

// memCtl ties the memory store to a CLI session.
type memCtl struct {
	store *memory.Store
	ix    atomic.Pointer[memory.Index]
	cwd   string
	skip  string // current session id: its content is already in context
	ready chan struct{}
	once  sync.Once
}

func memoryWanted(cfg config.Config) bool { return cfg.Memory == nil || *cfg.Memory }

func openMemory(cfg config.Config, cwd string) *memCtl {
	if !memoryWanted(cfg) {
		return nil
	}
	s, err := memory.Open(config.Home(), config.ProjectRoot(cwd))
	if err != nil {
		return nil
	}
	return &memCtl{store: s, cwd: cwd, ready: make(chan struct{})}
}

// buildIndex indexes decisions, journal and past sessions of this
// directory. It runs in the background; recall is skipped until ready.
func (m *memCtl) buildIndex() {
	docs := m.store.Docs()
	sessions, _ := session.ForCwd(m.cwd, 50)
	for _, s := range sessions {
		if s.ID == m.skip {
			continue
		}
		date := s.Updated.Format("2006-01-02")
		for _, msg := range s.Messages {
			if msg.Text == "" || msg.Role == provider.RoleTool || strings.HasPrefix(msg.Text, "[agentium]") {
				continue
			}
			text := msg.Text
			// Strip injected recall blocks so old recalls don't echo forever.
			if i := strings.Index(text, "</recall>"); i >= 0 {
				text = strings.TrimSpace(text[i+len("</recall>"):])
			}
			for len(text) > 0 {
				chunk := text
				if len(chunk) > 800 {
					chunk = chunk[:800]
				}
				text = text[len(chunk):]
				docs = append(docs, memory.Doc{Source: "session " + date + " " + string(msg.Role), Text: chunk})
			}
		}
	}
	m.ix.Store(memory.NewIndex(docs))
	m.once.Do(func() { close(m.ready) })
}

// Recall is pushed only when it is likely relevant: low-precision
// context injected automatically is neutral or harmful (CodeGrep,
// VibeMemBench, 2026). The model can pull more with search {memory}.
const (
	recallMinTerms    = 2   // distinct query terms a hit must contain
	recallMinCoverage = 0.5 // and at least this share of them
	recallMaxHits     = 2
	recallChars       = 700
)

// recall returns a <recall> block for input and the number of hits.
func (m *memCtl) recall(input string) (string, int) {
	// The first index build runs in the background from startup; give it
	// a brief moment rather than skipping recall on the first turn.
	select {
	case <-m.ready:
	case <-time.After(150 * time.Millisecond):
	}
	ix := m.ix.Load()
	if ix == nil || len(strings.Fields(input)) < 3 {
		return "", 0
	}
	var hits []memory.Hit
	for _, h := range ix.Search(input, 8) {
		if h.Matched < recallMinTerms || h.Coverage < recallMinCoverage {
			continue
		}
		// Keep a second hit only when it is nearly as strong as the first.
		if len(hits) > 0 && h.Score < 0.6*hits[0].Score {
			break
		}
		hits = append(hits, h)
		if len(hits) == recallMaxHits {
			break
		}
	}
	return memory.Format(hits, recallChars), len(hits)
}

// search answers search {memory:"..."}: past decisions, journal entries
// (including errors hit before) and past sessions, verbatim.
func (m *memCtl) search(q string) string {
	select {
	case <-m.ready:
	case <-time.After(2 * time.Second):
	}
	ix := m.ix.Load()
	if ix == nil {
		return "(memory index not ready)"
	}
	hits := ix.Search(q, 6)
	if len(hits) == 0 {
		return "(nothing in memory matches; try other words)"
	}
	return memory.Format(hits, 2400)
}

// afterTurn applies memory directives from the replies and journals the
// turn (deterministically, no LLM call), then refreshes the index. When
// the turn read web or MCP content, directives are held for review.
func (m *memCtl) afterTurn(prompt string, replies, files, errs, lessons []string, untrusted bool, report func(string)) {
	ds := memory.Parse(strings.Join(replies, "\n"))
	var reps []string
	if untrusted {
		reps = m.store.Hold(ds)
	} else {
		reps = m.store.Apply(ds)
	}
	// Promote before journaling this turn, so "seen before" means an
	// earlier turn.
	reps = append(reps, m.store.Lessons(lessons, !untrusted)...)
	for _, r := range reps {
		report("· " + r)
	}
	last := ""
	if len(replies) > 0 {
		last = replies[len(replies)-1]
	}
	entry := "user: " + oneLine(memory.Redact(prompt), 240)
	if len(files) > 0 {
		entry += "\nfiles: " + strings.Join(limitList(files, 12), ", ")
	}
	if len(errs) > 0 {
		// Errors and what fixed them are the lessons worth recalling.
		entry += "\nerrors: " + memory.Redact(strings.Join(limitList(errs, 4), " | "))
	}
	if len(lessons) > 0 {
		entry += "\nlessons: " + memory.Redact(strings.Join(limitList(lessons, 4), " | "))
	}
	if last != "" {
		entry += "\nresult: " + oneLine(memory.Redact(last), 320)
	}
	_ = m.store.Journal(entry)
	go m.buildIndex()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}

const tidyPrompt = `You maintain an AI coding agent's long-term memory. Rewrite it to be accurate, deduplicated and compact.

Rules:
- Merge duplicates and near-duplicates; drop facts that are obsolete, contradicted by newer entries or decisions, or trivial.
- Promote durable facts from the journal (conventions, commands, lessons from mistakes, "@pending" entries) when worth keeping.
- USER.md holds user-wide preferences only; MEMORY.md holds facts about this project.
- One fact per line, starting with "- ". USER.md at most 900 characters, MEMORY.md at most 1800.
- Keep a trailing "(YYYY-MM-DD · path)" suffix as it is; drop entries listed as stale unless they are still true.
- Never include secrets.

Reply with exactly this format and nothing else:
=== USER.md
- ...
=== MEMORY.md
- ...`

func cmdTidy(args []string) error {
	fs := flag.NewFlagSet("tidy", flag.ContinueOnError)
	modelRef := fs.String("m", "", "model to use (default: fast_model, then model)")
	yes := fs.Bool("yes", false, "write without asking")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	st, err := memory.Open(config.Home(), config.ProjectRoot(cwd))
	if err != nil {
		return err
	}
	ref := firstNonEmpty(*modelRef, cfg.FastModel, os.Getenv("AGENTIUM_MODEL"), cfg.Model)
	res, err := provider.Resolve(ref, cfg, auth)
	if err != nil {
		return err
	}
	user, mem := readFile(st.UserPath), readFile(st.MemoryPath)
	var in strings.Builder
	fmt.Fprintf(&in, "=== USER.md (current)\n%s\n=== MEMORY.md (current)\n%s\n=== DECISIONS\n", user, mem)
	for _, d := range st.Decisions() {
		fmt.Fprintf(&in, "%s %s [%s] %s\n", d.ID, d.Date, d.Status, d.Text)
	}
	if stale := st.Stale(); len(stale) > 0 {
		in.WriteString("=== STALE (hidden from the agent)\n" + strings.Join(stale, "\n") + "\n")
	}
	in.WriteString("=== JOURNAL (recent)\n")
	entries := st.JournalEntries()
	if len(entries) > 60 {
		entries = entries[len(entries)-60:]
	}
	for _, e := range entries {
		in.WriteString(e + "\n\n")
	}
	fmt.Fprintf(os.Stderr, "tidying memory with %s/%s …\n", res.Provider, res.Model)
	resp, err := res.Client.Stream(context.Background(), provider.Request{
		Model: res.Model, System: "You curate concise, accurate memory files.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Text: in.String() + "\n---\n" + tidyPrompt}},
		MaxTokens: 3000,
	}, nil)
	if err != nil {
		return err
	}
	newUser, newMem, err := parseTidy(resp.Text)
	if err != nil {
		return err
	}
	newUser, newMem = memory.Redact(newUser), memory.Redact(newMem)
	if len(newUser) > memory.UserLimit || len(newMem) > memory.MemoryLimit {
		return fmt.Errorf("model output exceeds the size caps (%d/%d, %d/%d chars); try again", len(newUser), memory.UserLimit, len(newMem), memory.MemoryLimit)
	}
	changed := printDiff("USER.md", user, newUser) + printDiff("MEMORY.md", mem, newMem)
	if changed == 0 {
		fmt.Fprintln(os.Stderr, "memory is already tidy")
		return nil
	}
	if !*yes {
		if !isTTY(os.Stdin) {
			return errors.New("not a terminal: pass --yes to write")
		}
		fmt.Fprint(os.Stderr, "write these changes? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(os.Stderr, "unchanged")
			return nil
		}
	}
	unlock := st.Lock()
	// A running session may have written memory while the model and the
	// user were deciding: then the reviewed diff is stale.
	if readFile(st.UserPath) != user || readFile(st.MemoryPath) != mem {
		unlock()
		return errors.New("memory changed meanwhile (another session wrote to it); run tidy again")
	}
	for _, f := range [][2]string{{st.UserPath, newUser}, {st.MemoryPath, newMem}} {
		if old := readFile(f[0]); old != "" {
			_ = fsx.WriteFile(f[0]+".bak", []byte(old), 0o600)
		}
		if err := fsx.WriteFile(f[0], []byte(f[1]), 0o600); err != nil {
			unlock()
			return err
		}
	}
	unlock()
	_ = st.Journal("tidy: memory consolidated")
	fmt.Fprintln(os.Stderr, "memory updated (backups: *.bak)")
	return nil
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func parseTidy(s string) (user, mem string, err error) {
	_, rest, ok := strings.Cut(s, "=== USER.md")
	if !ok {
		return "", "", errors.New("unexpected tidy output (no USER.md section)")
	}
	user, mem, ok = strings.Cut(rest, "=== MEMORY.md")
	if !ok {
		return "", "", errors.New("unexpected tidy output (no MEMORY.md section)")
	}
	clean := func(x string) string {
		var lines []string
		for _, l := range strings.Split(x, "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "- ") && len(l) > 2 {
				lines = append(lines, l)
			}
		}
		return strings.Join(lines, "\n")
	}
	return clean(user), clean(mem), nil
}

// printDiff shows removed/added lines and returns how many changed.
func printDiff(name, old, new string) int {
	oldSet, newSet := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(old, "\n") {
		oldSet[strings.TrimSpace(l)] = true
	}
	for _, l := range strings.Split(new, "\n") {
		newSet[strings.TrimSpace(l)] = true
	}
	n := 0
	header := func() {
		if n == 0 {
			fmt.Fprintf(os.Stderr, "\n%s\n", name)
		}
	}
	for _, l := range strings.Split(old, "\n") {
		if l = strings.TrimSpace(l); l != "" && !newSet[l] {
			header()
			fmt.Fprintln(os.Stderr, "  - "+l)
			n++
		}
	}
	for _, l := range strings.Split(new, "\n") {
		if l = strings.TrimSpace(l); l != "" && !oldSet[l] {
			header()
			fmt.Fprintln(os.Stderr, "  + "+l)
			n++
		}
	}
	return n
}
