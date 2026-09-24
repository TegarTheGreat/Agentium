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
	"github.com/tegarthegreat/agentium/internal/fsx"
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
	out, err := s.gitRaw(ctx, args...)
	return strings.TrimSpace(out), err
}

func (s *Store) gitRaw(ctx context.Context, args ...string) (string, error) {
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
	return out.String(), nil
}

// lock serializes this store's git operations across processes (two
// sessions in one workspace share it) and clears an index.lock left by a
// git process that was killed (it would block every later snapshot).
func (s *Store) lock() func() {
	unlock := fsx.Lock(filepath.Join(s.GitDir, "agentium.lock"), 3*time.Minute)
	il := filepath.Join(s.GitDir, "index.lock")
	if st, err := os.Stat(il); err == nil && time.Since(st.ModTime()) > 2*time.Minute {
		_ = os.Remove(il)
	}
	return unlock
}

// Snapshot records the current state of the workspace and returns its id.
func (s *Store) Snapshot(ctx context.Context, msg string) (string, error) {
	defer s.lock()()
	return s.snapshot(ctx, msg)
}

// maxHistory is how many snapshots are kept before old ones are dropped
// (sessions reference only their recent checkpoints).
const maxHistory = 400

func (s *Store) snapshot(ctx context.Context, msg string) (string, error) {
	if _, err := s.git(ctx, "add", "-A", "--ignore-errors", "."); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "commit", "-q", "--allow-empty", "--no-verify", "-m", msg); err != nil {
		return "", err
	}
	id, err := s.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if n, _ := s.git(ctx, "rev-list", "--count", "HEAD"); len(n) > 0 {
		if c := atoi(n); c > maxHistory {
			s.trim(ctx)
		}
	}
	return id, nil
}

// trim keeps the storage bounded: history restarts from the current
// state (a parentless commit) and unreachable objects are collected.
// Undo of checkpoints older than that reports them as gone.
func (s *Store) trim(ctx context.Context) {
	tree, err := s.git(ctx, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return
	}
	c, err := s.git(ctx, "commit-tree", tree, "-m", "history trimmed")
	if err != nil {
		return
	}
	if _, err := s.git(ctx, "update-ref", "HEAD", c); err != nil {
		return
	}
	_, _ = s.git(ctx, "reflog", "expire", "--expire=now", "--all")
	_, _ = s.git(ctx, "gc", "--prune=now", "--quiet")
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// diffPaths returns (status, path) pairs between two snapshots, parsed
// from NUL-separated output so any file name round-trips.
func (s *Store) diffPaths(ctx context.Context, from, to string) ([][2]string, error) {
	out, err := s.gitRaw(ctx, "diff", "-z", "--name-status", "--no-renames", from, to)
	if err != nil {
		return nil, err
	}
	f := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var pairs [][2]string
	for i := 0; i+1 < len(f); i += 2 {
		pairs = append(pairs, [2]string{f[i], f[i+1]})
	}
	return pairs, nil
}

// Restore undoes a turn. With after (the snapshot taken when the turn
// ended), only the files that turn changed are put back, so edits made
// since by the user, their editor or another session are kept; without
// it, the whole workspace returns to snapshot id. Files the turn created
// are removed. It returns the paths it touched.
func (s *Store) Restore(ctx context.Context, id, after string) ([]string, error) {
	defer s.lock()()
	if _, err := s.git(ctx, "cat-file", "-e", id+"^{commit}"); err != nil {
		return nil, errors.New("that checkpoint is no longer stored (history was trimmed)")
	}
	cur, err := s.snapshot(ctx, "before undo")
	if err != nil {
		return nil, err
	}
	to := cur
	if after != "" {
		if _, err := s.git(ctx, "cat-file", "-e", after+"^{commit}"); err == nil {
			to = after
		}
	}
	pairs, err := s.diffPaths(ctx, id, to)
	if err != nil {
		return nil, err
	}
	var touched, checkout []string
	for _, p := range pairs {
		st, path := p[0], p[1]
		touched = append(touched, path)
		if st == "A" {
			if err := os.Remove(filepath.Join(s.Root, path)); err != nil && !os.IsNotExist(err) {
				return touched, err
			}
			removeEmptyParents(s.Root, filepath.Dir(filepath.Join(s.Root, path)))
		} else {
			checkout = append(checkout, path)
		}
	}
	for len(checkout) > 0 {
		n := min(len(checkout), 200)
		args := append([]string{"checkout", id, "--"}, checkout[:n]...)
		if _, err := s.git(ctx, args...); err != nil {
			return touched, err
		}
		checkout = checkout[n:]
	}
	return touched, nil
}

// Changed lists files that differ between snapshot id and now.
func (s *Store) Changed(ctx context.Context, id string) ([]string, error) {
	defer s.lock()()
	cur, err := s.snapshot(ctx, "diff")
	if err != nil {
		return nil, err
	}
	pairs, err := s.diffPaths(ctx, id, cur)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pairs {
		out = append(out, p[1])
	}
	return out, nil
}

// Nested lists directories that are git repositories of their own
// (submodules, vendored clones): their contents are not in checkpoints.
func (s *Store) Nested(ctx context.Context) []string {
	out, err := s.gitRaw(ctx, "ls-files", "-s", "-z")
	if err != nil {
		return nil
	}
	var nested []string
	for _, e := range strings.Split(out, "\x00") {
		if strings.HasPrefix(e, "160000 ") {
			if _, path, ok := strings.Cut(e, "\t"); ok {
				nested = append(nested, path)
			}
		}
	}
	return nested
}

func removeEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
