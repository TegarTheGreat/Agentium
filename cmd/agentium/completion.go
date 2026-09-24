package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/skill"
)

// Suggestions for the line editor: /commands (with what they do) and
// @file mentions (fuzzy-matched against the project's files).

type slashCmd struct {
	name, hint string
	args       bool // takes arguments: Enter completes instead of running
}

var slashCommands = []slashCmd{
	{"/model", "choose a model", false},
	{"/login", "add or change a provider's API key", false},
	{"/logout", "remove a provider's stored key", true},
	{"/mode", "approvals: ask · auto · yolo · plan (also shift+tab)", false},
	{"/effort", "reasoning effort", false},
	{"/plan", "investigate read-only, then propose a plan", false},
	{"/go", "carry out the plan", false},
	{"/undo", "revert the last turn's file changes", false},
	{"/rewind", "revert file changes back to an earlier turn (esc esc)", false},
	{"/copy", "copy the last reply to the clipboard", false},
	{"/diff", "what changed in the working tree", false},
	{"/context", "what fills the context window", false},
	{"/compact", "summarize the older conversation to free context", false},
	{"/btw", "ask a side question (not added to the conversation)", true},
	{"/theme", "auto · dark · light", false},
	{"/memory", "what I remember, and where (edit to change it)", false},
	{"/sessions", "list saved conversations", false},
	{"/resume", "continue a saved conversation", true},
	{"/clear", "start a new conversation", false},
	{"/usage", "tokens used in this session", false},
	{"/skills", "list skills", false},
	{"/config", "show current settings", false},
	{"/update", "install the latest release", false},
	{"/help", "commands and keys (also ?)", false},
	{"/exit", "quit (also ctrl+d)", false},
}

type completer struct {
	root   string
	skills []skill.Skill

	mu     sync.Mutex
	files  []string
	listed time.Time
}

func (c *completer) complete(before []rune) ([]suggestion, int) {
	s := string(before)
	if strings.HasPrefix(s, "/") && !strings.ContainsAny(s, " \n") {
		return c.commands(s), 0
	}
	i := strings.LastIndexAny(s, " \n\t(")
	tok := s[i+1:]
	if !strings.HasPrefix(tok, "@") {
		return nil, 0
	}
	start := len([]rune(s[:i+1]))
	return c.fileSuggestions(tok[1:]), start
}

func (c *completer) commands(prefix string) []suggestion {
	var out []suggestion
	for _, cmd := range slashCommands {
		if strings.HasPrefix(cmd.name, prefix) {
			insert := cmd.name
			if cmd.args {
				insert += " "
			}
			out = append(out, suggestion{insert: insert, label: cmd.name, hint: cmd.hint, run: !cmd.args})
		}
	}
	for _, sk := range c.skills {
		name := "/" + sk.Name
		if strings.HasPrefix(name, prefix) {
			out = append(out, suggestion{insert: name + " ", label: name, hint: "skill · " + firstLine(sk.Description)})
		}
	}
	return out
}

// fileSuggestions ranks the project's files against a fuzzy query.
func (c *completer) fileSuggestions(q string) []suggestion {
	files := c.projectFiles()
	type hit struct {
		path  string
		score int
	}
	var hits []hit
	lq := strings.ToLower(q)
	for _, f := range files {
		if sc, ok := fuzzyScore(strings.ToLower(f), lq); ok {
			hits = append(hits, hit{f, sc})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return len(hits[i].path) < len(hits[j].path)
	})
	var out []suggestion
	for i, h := range hits {
		if i == 50 {
			break
		}
		dir := filepath.Dir(h.path)
		if dir == "." {
			dir = ""
		}
		out = append(out, suggestion{insert: "@" + h.path + " ", label: filepath.Base(h.path), hint: dir})
	}
	return out
}

// fuzzyScore matches q as a subsequence of s; contiguous runs, matches in
// the file name and at word starts score higher.
func fuzzyScore(s, q string) (int, bool) {
	if q == "" {
		return 0, true
	}
	base := strings.LastIndex(s, "/") + 1
	score, qi, run := 0, 0, 0
	qr := []rune(q)
	prev := rune('/')
	for i, r := range s {
		if qi < len(qr) && r == qr[qi] {
			qi++
			run++
			score += 1 + run*2
			if i >= base {
				score += 3
			}
			if strings.ContainsRune("/._- ", prev) {
				score += 4
			}
		} else {
			run = 0
		}
		prev = r
	}
	if qi < len(qr) {
		return 0, false
	}
	if strings.Contains(s[base:], q) {
		score += 20
	}
	return score, true
}

// projectFiles lists the project's files (git's view when available,
// with a time limit so a huge repository cannot freeze typing), refreshed
// at most every 30 seconds.
func (c *completer) projectFiles() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.listed) < 30*time.Second && c.files != nil {
		return c.files
	}
	c.listed = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = c.root
	if out, err := cmd.Output(); err == nil {
		c.files = strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
		if len(c.files) > 20000 {
			c.files = c.files[:20000]
		}
		return c.files
	}
	var files []string
	filepath.WalkDir(c.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(files) >= 5000 {
			return filepath.SkipDir
		}
		name := d.Name()
		if d.IsDir() && p != c.root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			if rel, err := filepath.Rel(c.root, p); err == nil {
				files = append(files, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	c.files = files
	return files
}
