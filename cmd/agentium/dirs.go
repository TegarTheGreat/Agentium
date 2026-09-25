package main

import (
	"fmt"
	"github.com/tegarthegreat/agentium/internal/config"
	"os"
	"path/filepath"
	"strings"
)

// dirList is a repeatable --add-dir flag.
type dirList []string

func (d *dirList) String() string     { return strings.Join(*d, ",") }
func (d *dirList) Set(v string) error { *d = append(*d, v); return nil }

// resolveDirs turns paths (~ and relative ones too) into existing,
// symlink-free absolute directories.
func resolveDirs(paths []string, cwd string) ([]string, error) {
	var out []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, strings.TrimPrefix(p, "~"))
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("add-dir: %s is not a directory", p)
		}
		if tooBroad(p) {
			return nil, fmt.Errorf("add-dir: %s is too broad (it holds your home or agentium's settings); add the project folder instead", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// dirsPrompt tells the model about the added working directories.
func dirsPrompt(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	return "\n\n# More working directories\nThe user added these as workspaces too; read, search and change files there as in the main one (use absolute paths):\n- " + strings.Join(dirs, "\n- ")
}

// tooBroad reports whether dir is /, or holds the home folder or
// agentium's own settings (a way to rewrite its config unasked).
func tooBroad(dir string) bool {
	dir = filepath.Clean(dir)
	if dir == filepath.Dir(dir) {
		return true // a filesystem root
	}
	var guard []string
	if h, err := os.UserHomeDir(); err == nil {
		guard = append(guard, h)
	}
	guard = append(guard, config.Home())
	for _, g := range guard {
		if r, err := filepath.EvalSymlinks(g); err == nil {
			g = r
		}
		if rel, err := filepath.Rel(dir, filepath.Clean(g)); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}
