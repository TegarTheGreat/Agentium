package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
)

// --worktree NAME runs the session in its own git worktree (branch
// agentium/NAME), so several sessions can work on one repository at once
// without touching each other's files. The worktree lives in agentium's
// data folder, not in the repository.

var worktreeName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,60}$`)

// gitOut is git's trimmed output (see git in bestof.go).
func gitOut(dir string, args ...string) (string, error) {
	out, err := git(dir, args...)
	return strings.TrimSpace(out), err
}

// enterWorktree creates (or reuses) the worktree and returns the folder
// to work in (the same subfolder of the repository as cwd), and a
// function to call on exit.
func enterWorktree(cwd, name string) (dir string, done func() string, err error) {
	if name == "auto" || name == "" {
		name = time.Now().Format("0102-1504")
	}
	if !worktreeName.MatchString(name) || strings.HasPrefix(name, ".") {
		return "", nil, fmt.Errorf("worktree name %q: use letters, digits, . _ -", name)
	}
	top, err := gitOut(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, fmt.Errorf("--worktree needs a git repository")
	}
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r // macOS: /var is /private/var, as git reports it
	}
	rel, _ := filepath.Rel(top, cwd)
	h := sha256.Sum256([]byte(top))
	path := filepath.Join(config.Home(), "worktrees", filepath.Base(top)+"-"+hex.EncodeToString(h[:4]), name)
	branch := "agentium/" + name
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", nil, err
		}
		if _, err := gitOut(top, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			_, err = gitOut(top, "worktree", "add", path, branch)
			if err != nil {
				return "", nil, err
			}
		} else if _, err := gitOut(top, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
			return "", nil, err
		}
	}
	base, _ := gitOut(path, "rev-parse", "HEAD")
	dir = filepath.Join(path, rel)
	if _, err := os.Stat(dir); err != nil {
		dir = path
	}
	done = func() string {
		status, err := gitOut(path, "status", "--porcelain")
		head, _ := gitOut(path, "rev-parse", "HEAD")
		if err == nil && status == "" && head == base {
			// Nothing done here: leave no worktree or branch behind.
			if _, err := gitOut(top, "worktree", "remove", path); err == nil {
				_, _ = gitOut(top, "branch", "-D", branch)
				return ""
			}
		}
		return fmt.Sprintf("worktree kept: %s (branch %s)\n  merge it with: git merge %s · remove it with: git worktree remove %s", path, branch, branch, path)
	}
	return dir, done, nil
}

// gitCommonDir is the repository's shared .git folder when root is a
// linked worktree (it lies outside root), else "".
func gitCommonDir(root string) string {
	common, err := gitOut(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return ""
	}
	if rel, err := filepath.Rel(root, common); err == nil && !strings.HasPrefix(rel, "..") {
		return "" // inside the workspace already
	}
	return common
}
