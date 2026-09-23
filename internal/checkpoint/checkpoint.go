// Package checkpoint snapshots the workspace in a shadow git repository
// (outside the project, never touching its own .git) so any turn can be
// undone, including changes made by shell commands.
package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Store is a shadow repository for one workspace.
type Store struct {
	GitDir string
	Root   string
}

// ErrNoGit means git is not installed; checkpoints are disabled.
var ErrNoGit = errors.New("git not found: checkpoints disabled")

// Open returns the store for root under base, creating it on first use.
func Open(base, root string) (*Store, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, ErrNoGit
	}
	h := sha256.Sum256([]byte(root))
	s := &Store{GitDir: filepath.Join(base, hex.EncodeToString(h[:8])), Root: root}
	if _, err := os.Stat(filepath.Join(s.GitDir, "HEAD")); err != nil {
		if err := os.MkdirAll(s.GitDir, 0o700); err != nil {
			return nil, err
		}
		if out, err := exec.Command("git", "init", "-q", "--bare", s.GitDir).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git init: %v: %s", err, strings.TrimSpace(string(out)))
		}
		// Heavy, regenerable directories are skipped even if the project
		// forgot to ignore them.
		excl := "node_modules/\n.venv/\nvenv/\n__pycache__/\ntarget/\ndist/\nbuild/\n.next/\n.cache/\n*.log\n"
		_ = os.MkdirAll(filepath.Join(s.GitDir, "info"), 0o700)
		_ = os.WriteFile(filepath.Join(s.GitDir, "info", "exclude"), []byte(excl), 0o600)
		_ = os.WriteFile(filepath.Join(s.GitDir, "agentium-root"), []byte(root+"\n"), 0o600)
		for _, kv := range [][2]string{{"core.autocrlf", "false"}, {"core.bare", "false"}, {"gc.auto", "0"}} {
			_, _ = s.git(context.Background(), "config", kv[0], kv[1])
		}
	}
	return s, nil
}

func (s *Store) git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	full := append([]string{"--git-dir=" + s.GitDir, "--work-tree=" + s.Root, "-c", "core.quotepath=off"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = s.Root
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=agentium", "GIT_AUTHOR_EMAIL=agentium@localhost",
		"GIT_COMMITTER_NAME=agentium", "GIT_COMMITTER_EMAIL=agentium@localhost",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Snapshot records the current state of the workspace and returns its id.
func (s *Store) Snapshot(ctx context.Context, msg string) (string, error) {
	if _, err := s.git(ctx, "add", "-A", "--ignore-errors", "."); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "commit", "-q", "--allow-empty", "--no-verify", "-m", msg); err != nil {
		return "", err
	}
	return s.git(ctx, "rev-parse", "HEAD")
}

// Restore puts the workspace back to snapshot id: files changed or deleted
// since are restored, files created since are removed. It returns the
// paths it touched.
func (s *Store) Restore(ctx context.Context, id string) ([]string, error) {
	cur, err := s.Snapshot(ctx, "before undo")
	if err != nil {
		return nil, err
	}
	diff, err := s.git(ctx, "diff", "--name-status", "--no-renames", id, cur)
	if err != nil {
		return nil, err
	}
	var touched []string
	needCheckout := false
	for _, line := range strings.Split(diff, "\n") {
		st, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		touched = append(touched, path)
		if st == "A" {
			if err := os.Remove(filepath.Join(s.Root, path)); err != nil && !os.IsNotExist(err) {
				return touched, err
			}
			removeEmptyParents(s.Root, filepath.Dir(filepath.Join(s.Root, path)))
		} else {
			needCheckout = true
		}
	}
	if !needCheckout {
		return touched, nil
	}
	if _, err := s.git(ctx, "checkout", id, "--", "."); err != nil {
		return touched, err
	}
	return touched, nil
}

// Changed lists files that differ between snapshot id and now.
func (s *Store) Changed(ctx context.Context, id string) ([]string, error) {
	cur, err := s.Snapshot(ctx, "diff")
	if err != nil {
		return nil, err
	}
	out, err := s.git(ctx, "diff", "--name-only", "--no-renames", id, cur)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

func removeEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
