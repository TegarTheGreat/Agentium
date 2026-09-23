package tool

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// gitGuard protects the repository's own configuration from shell
// commands. The sandbox must leave .git writable (commits, branches), but
// a line in .git/config such as core.fsmonitor or a new hook runs later,
// outside the sandbox, the next time anyone uses git. Before each command
// the guard snapshots .git/config and the hooks; afterwards it undoes any
// change that adds a command-running setting or hook.
type gitGuard struct {
	dir    string // .git directory ("" when none)
	config []byte
	hooks  map[string][]byte
}

// dangerousGit matches config keys whose value is a program git runs.
var dangerousGit = regexp.MustCompile(`(?i)^\s*(fsmonitor|hookspath|sshcommand|pager|editor|askpass|textconv|clean|smudge|process|helper|gitproxy|program|cmd|path|command)\s*=|^\s*[\w.-]+\s*=\s*"?!`)

func snapGit(root string) *gitGuard {
	dir := ""
	for d := root; ; {
		if st, err := os.Stat(filepath.Join(d, ".git")); err == nil && st.IsDir() {
			dir = filepath.Join(d, ".git")
			break
		}
		p := filepath.Dir(d)
		if p == d {
			return &gitGuard{}
		}
		d = p
	}
	g := &gitGuard{dir: dir, hooks: map[string][]byte{}}
	g.config, _ = os.ReadFile(filepath.Join(dir, "config"))
	for _, h := range hookFiles(dir) {
		g.hooks[h], _ = os.ReadFile(filepath.Join(dir, "hooks", h))
	}
	return g
}

func hookFiles(dir string) []string {
	ents, _ := os.ReadDir(filepath.Join(dir, "hooks"))
	var out []string
	for _, e := range ents {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".sample") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func dangerousLines(cfg []byte) map[string]bool {
	out := map[string]bool{}
	section := ""
	for _, l := range strings.Split(string(cfg), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = strings.ToLower(t)
			continue
		}
		if dangerousGit.MatchString(t) {
			out[section+" "+strings.ToLower(t)] = true
		}
	}
	return out
}

// check undoes dangerous changes and describes what it undid ("" if none).
func (g *gitGuard) check() string {
	if g == nil || g.dir == "" {
		return ""
	}
	var undone []string
	cfgPath := filepath.Join(g.dir, "config")
	now, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(now, g.config) {
		before := dangerousLines(g.config)
		for l := range dangerousLines(now) {
			if !before[l] {
				if os.WriteFile(cfgPath, g.config, 0o644) == nil {
					undone = append(undone, ".git/config ("+strings.TrimSpace(l)+")")
				}
				break
			}
		}
	}
	for _, h := range hookFiles(g.dir) {
		p := filepath.Join(g.dir, "hooks", h)
		cur, _ := os.ReadFile(p)
		old, existed := g.hooks[h]
		switch {
		case !existed:
			if os.Remove(p) == nil {
				undone = append(undone, "new hook .git/hooks/"+h)
			}
		case !bytes.Equal(cur, old):
			if os.WriteFile(p, old, 0o755) == nil {
				undone = append(undone, "changed hook .git/hooks/"+h)
			}
		}
	}
	if len(undone) == 0 {
		return ""
	}
	return "\n[agentium: undid " + strings.Join(undone, ", ") + ": git would run it later outside the sandbox. Ask the user to make such changes.]"
}
