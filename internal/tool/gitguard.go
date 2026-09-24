package tool

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// gitGuard protects the repository's configuration from shell commands.
// The sandbox must leave .git writable (commits, branches), but a setting
// such as core.fsmonitor or a new hook runs later, outside the sandbox,
// the next time anyone uses git. The guard snapshots every config file
// and hook git reads (main repo, linked worktrees, submodules) and undoes
// any change that adds a command-running setting or hook. It checks
// after each command and again before the next one, so a background
// process that changes git later is caught too.
type gitGuard struct {
	configs map[string][]byte // config files and their content
	hooks   map[string][]byte // hook files and their content
	dirs    []string          // hook directories
}

// commandKeys are config keys (last component) whose value git runs.
var commandKeys = map[string]bool{
	"fsmonitor": true, "hookspath": true, "sshcommand": true, "pager": true, "editor": true, "askpass": true,
	"textconv": true, "clean": true, "smudge": true, "process": true, "helper": true, "gitproxy": true,
	"program": true, "cmd": true, "external": true, "driver": true, "difffilter": true, "uploadpack": true,
	"receivepack": true, "alternaterefscommand": true, "sequenceeditor": true,
}

func dangerousKey(key, val string) bool {
	k := strings.ToLower(key)
	first, _, _ := strings.Cut(k, ".")
	last := k[strings.LastIndexByte(k, '.')+1:]
	v := strings.TrimSpace(val)
	switch {
	case first == "pager", first == "include", first == "includeif":
		return true
	case first == "alias" || last == "update":
		return strings.HasPrefix(v, "!")
	}
	return commandKeys[last] || strings.HasSuffix(last, "command")
}

// gitDirs finds the git directories that affect root: its own, the
// common one (linked worktrees), and submodules' and worktrees' dirs.
func gitDirs(root string) []string {
	var gd string
	for d := root; gd == ""; {
		p := filepath.Join(d, ".git")
		if st, err := os.Stat(p); err == nil {
			if st.IsDir() {
				gd = p
			} else if b, err := os.ReadFile(p); err == nil {
				t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
				if !filepath.IsAbs(t) {
					t = filepath.Join(d, t)
				}
				gd = t
			}
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			return nil
		}
		d = parent
	}
	if gd == "" {
		return nil
	}
	dirs := []string{gd}
	common := gd
	if b, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gd, c)
		}
		common = filepath.Clean(c)
		dirs = append(dirs, common)
	}
	for _, sub := range []string{"modules", "worktrees"} {
		base := filepath.Join(common, sub)
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if strings.Count(strings.TrimPrefix(p, base), string(filepath.Separator)) > 6 {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "HEAD")); err == nil && p != base {
				dirs = append(dirs, p)
			}
			return nil
		})
	}
	return dirs
}

func snapGit(root string) *gitGuard {
	g := &gitGuard{configs: map[string][]byte{}, hooks: map[string][]byte{}}
	for _, d := range gitDirs(root) {
		for _, f := range []string{"config", "config.worktree"} {
			p := filepath.Join(d, f)
			b, err := os.ReadFile(p)
			if err == nil {
				g.configs[p] = b
			} else {
				g.configs[p] = nil // absent: creating it is a change too
			}
		}
		hd := filepath.Join(d, "hooks")
		g.dirs = append(g.dirs, hd)
		for _, h := range hookFiles(hd) {
			g.hooks[h], _ = os.ReadFile(h)
		}
	}
	// A core.hooksPath already in use (Husky, lefthook) points git at hooks
	// in the workspace, and included config files are read like config.
	hooks, includes := GitExtras(root)
	for _, hd := range hooks {
		g.dirs = append(g.dirs, hd)
		for _, h := range hookFiles(hd) {
			g.hooks[h], _ = os.ReadFile(h)
		}
	}
	for _, p := range includes {
		if _, seen := g.configs[p]; !seen {
			g.configs[p], _ = os.ReadFile(p)
		}
	}
	return g
}

// GitExtras returns the hook directories set by core.hooksPath and the
// config files pulled in by include/includeIf, for the repository at
// root, as git itself resolves them.
func GitExtras(root string) (hookDirs, includes []string) {
	git, err := exec.LookPath("git")
	if err != nil || len(gitDirs(root)) == 0 {
		return nil, nil
	}
	cmd := exec.Command(git, "-C", root, "config", "--show-origin", "--null", "--list")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	b, err := cmd.Output()
	if err != nil {
		return nil, nil
	}
	fields := strings.Split(string(b), "\x00")
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	for i := 0; i+1 < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		k, v, _ := strings.Cut(entry, "\n")
		if file, ok := strings.CutPrefix(origin, "file:"); ok && file != "" {
			if !filepath.IsAbs(file) {
				file = filepath.Join(root, file)
			}
			file = filepath.Clean(file)
			if !seen[file] && !strings.Contains(filepath.ToSlash(file), "/.git/") {
				seen[file] = true
				includes = append(includes, file)
			}
		}
		if strings.EqualFold(k, "core.hookspath") && v != "" {
			if strings.HasPrefix(v, "~/") && home != "" {
				v = filepath.Join(home, v[2:])
			} else if !filepath.IsAbs(v) {
				v = filepath.Join(root, v)
			}
			hookDirs = append(hookDirs, filepath.Clean(v))
		}
	}
	return hookDirs, includes
}

func hookFiles(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".sample") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// dangerousEntries lists command-running settings in a config file,
// parsed by git itself when available (it knows every syntax variant).
func dangerousEntries(path string, content []byte) map[string]bool {
	out := map[string]bool{}
	if len(content) == 0 {
		return out
	}
	if git, err := exec.LookPath("git"); err == nil {
		tmp, err := os.CreateTemp("", "agentium-gitcfg-*")
		if err == nil {
			defer os.Remove(tmp.Name())
			tmp.Write(content)
			tmp.Close()
			cmd := exec.Command(git, "config", "-f", tmp.Name(), "--no-includes", "--list", "--null")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
			if b, err := cmd.Output(); err == nil {
				for _, e := range strings.Split(string(b), "\x00") {
					k, v, _ := strings.Cut(e, "\n")
					if k != "" && dangerousKey(k, v) {
						out[k+"="+v] = true
					}
				}
				return out
			}
		}
	}
	// Fallback (git missing or the file does not parse): any line that
	// looks like a command-running key counts, with or without section.
	for _, l := range strings.Split(string(content), "\n") {
		t := strings.TrimSpace(l)
		if i := strings.IndexByte(t, ']'); strings.HasPrefix(t, "[") && i >= 0 {
			t = strings.TrimSpace(t[i+1:])
		}
		k, v, ok := strings.Cut(t, "=")
		if ok && dangerousKey("x."+strings.TrimSpace(k), v) {
			out[strings.ToLower(t)] = true
		}
	}
	return out
}

// check undoes dangerous changes since the snapshot and describes what
// it undid ("" if none).
func (g *gitGuard) check() string {
	if g == nil {
		return ""
	}
	var undone []string
	for p, old := range g.configs {
		now, err := os.ReadFile(p)
		if err != nil || bytes.Equal(now, old) {
			continue
		}
		before := dangerousEntries(p, old)
		for e := range dangerousEntries(p, now) {
			if before[e] {
				continue
			}
			if old == nil {
				err = os.Remove(p)
			} else {
				err = os.WriteFile(p, old, 0o644)
			}
			if err == nil {
				undone = append(undone, rel(p)+" ("+e+")")
			}
			break
		}
	}
	for _, hd := range g.dirs {
		for _, h := range hookFiles(hd) {
			cur, _ := os.ReadFile(h)
			old, existed := g.hooks[h]
			switch {
			case !existed:
				if os.Remove(h) == nil {
					undone = append(undone, "new hook "+rel(h))
				}
			case !bytes.Equal(cur, old):
				if os.WriteFile(h, old, 0o755) == nil {
					undone = append(undone, "changed hook "+rel(h))
				}
			}
		}
	}
	if len(undone) == 0 {
		return ""
	}
	sort.Strings(undone)
	return "\n[agentium: undid " + strings.Join(undone, ", ") + ": git would run it later outside the sandbox. Ask the user to make such changes.]"
}

// rel shortens a path to start at its .git directory.
func rel(p string) string {
	if i := strings.LastIndex(p, ".git"+string(filepath.Separator)); i >= 0 {
		return p[i:]
	}
	return p
}
