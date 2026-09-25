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
	{"/model", "choose a model (alt+p)", false},
	{"/login", "add or change a provider's API key", false},
	{"/logout", "remove a provider's stored key", true},
	{"/mode", "approvals: ask · auto · yolo · plan (also shift+tab)", false},
	{"/effort", "reasoning effort (alt+t)", false},
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
	{"/vim", "vim keys in the message box (on/off)", false},
	{"/style", "how I talk: default · explanatory · learning · terse", false},
	{"/watch", "act on AI! and AI? comments you save in the code (on/off)", false},
	{"/memory", "what I remember, and where (edit to change it)", false},
	{"/export", "save the conversation as Markdown", false},
	{"/sessions", "list saved conversations", false},
	{"/resume", "continue a saved conversation (picker, number or name)", false},
	{"/rename", "name this conversation", true},
	{"/clear", "start a new conversation", false},
	{"/usage", "tokens and cost of this session (also /cost)", false},
	{"/skills", "list skills", false},
	{"/agents", "your specialist sub-agents", false},
	{"/bug", "open a prefilled bug report", true},
	{"/release-notes", "what changed in this version (or another)", false},
	{"/config", "show current settings", false},
	{"/update", "install the latest release", false},
	{"/doctor", "check the model, key, tools, terminal and MCP servers", false},
	{"/mcp", "MCP servers: status, tools, logs", false},
	{"/hooks", "the hooks in your settings", false},
	{"/tools", "the tools I can use", false},
	{"/permissions", "what runs without asking; revoke approvals", false},
	{"/add-dir", "work in another directory too", true},
	{"/init", "study the repo and write AGENTS.md", false},
	{"/review", "review the current changes for bugs", false},
	{"/security-review", "look for exploitable vulnerabilities in the changes", false},
	{"/commit", "commit the changes with a message in the repo's style", false},
	{"/pr", "push the branch and open a pull request", false},
	{"/fork", "continue in a copy of this conversation", false},
	{"/handoff", "start fresh with a brief for a new goal (instead of compacting)", true},
	{"/help", "commands and keys (also ?)", false},
	{"/exit", "quit (also ctrl+d)", false},
}

type completer struct {
	root      string
	skills    []skill.Skill
	skillList func() []skill.Skill // the current skills, when they can change
	cmds      []userCmd
	cmdsAt    time.Time // when cmds were read; zero: never re-read

	mu      sync.Mutex
	files   []string
	lower   []string // files, lower-cased
	listed  time.Time
	listing bool // a background refresh is running
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
	if !c.cmdsAt.IsZero() && time.Since(c.cmdsAt) > 2*time.Second {
		c.cmds, c.cmdsAt = userCommands(c.root), time.Now() // a command file added or changed
	}
	skills := c.skills
	if c.skillList != nil {
		skills = c.skillList()
	}
	for _, uc := range c.cmds {
		name := "/" + uc.name
		if strings.HasPrefix(name, prefix) {
			label := name
			if uc.hint != "" {
				label += " " + uc.hint
			}
			out = append(out, suggestion{insert: name + " ", label: label, hint: "command · " + firstLine(uc.desc)})
		}
	}
	for _, sk := range skills {
		name := "/" + sk.Name
		if strings.HasPrefix(name, prefix) {
			out = append(out, suggestion{insert: name + " ", label: name, hint: "skill · " + firstLine(sk.Description)})
		}
	}
	// What is typed in full comes first: Enter on /st runs /st, not /style.
	for i, sg := range out {
		if strings.TrimSpace(sg.insert) == prefix && i > 0 {
			copy(out[1:i+1], out[:i])
			out[0] = sg
			break
		}
	}
	return out
}

// fileSuggestions ranks the project's files against a fuzzy query.
func (c *completer) fileSuggestions(q string) []suggestion {
	files, lower := c.projectFiles()
	type hit struct {
		path  string
		score int
	}
	better := func(a, b hit) bool {
		if a.score != b.score {
			return a.score > b.score
		}
		return len(a.path) < len(b.path)
	}
	// The best 50, kept sorted: a full sort of every match is too slow
	// on a repository with 100k+ files.
	const keep = 50
	var top []hit
	qr := []rune(strings.ToLower(q))
	for i, f := range files {
		sc, ok := fuzzyRunes(lower[i], qr)
		if !ok {
			continue
		}
		h := hit{f, sc}
		if len(top) == keep && !better(h, top[keep-1]) {
			continue
		}
		at := sort.Search(len(top), func(j int) bool { return better(h, top[j]) })
		if len(top) < keep {
			top = append(top, hit{})
		}
		copy(top[at+1:], top[at:])
		top[at] = h
	}
	var out []suggestion
	for _, h := range top {
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
func fuzzyScore(s, q string) (int, bool) { return fuzzyRunes(s, []rune(q)) }

func fuzzyRunes(s string, qr []rune) (int, bool) {
	if len(qr) == 0 {
		return 0, true
	}
	base := strings.LastIndex(s, "/") + 1
	score, qi, run := 0, 0, 0
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
	if strings.Contains(s[base:], string(qr)) {
		score += 20
	}
	return score, true
}

// maxListed caps the files offered for @-completion.
const maxListed = 200000

// projectFiles lists the project's files (git's view when available).
// The first call lists them; later ones return the last list and refresh
// it in the background at most every 30 seconds, so a huge repository
// never stalls typing.
func (c *completer) projectFiles() (files, lower []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = listProject(c.root)
		c.lower, c.listed = lowered(c.files), time.Now()
	} else if time.Since(c.listed) > 30*time.Second && !c.listing {
		c.listing = true
		go func() {
			files := listProject(c.root)
			lower := lowered(files)
			c.mu.Lock()
			c.files, c.lower, c.listed, c.listing = files, lower, time.Now(), false
			c.mu.Unlock()
		}()
	}
	if len(c.lower) != len(c.files) { // set directly (tests)
		c.lower = lowered(c.files)
	}
	return c.files, c.lower
}

func lowered(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = strings.ToLower(f)
	}
	return out
}

func listProject(root string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
		if len(files) > maxListed {
			files = files[:maxListed]
		}
		return files
	}
	files := []string{}
	deadline := time.Now().Add(2 * time.Second)
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if len(files) >= maxListed || time.Now().After(deadline) {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() && p != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			if rel, err := filepath.Rel(root, p); err == nil {
				files = append(files, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	return files
}
