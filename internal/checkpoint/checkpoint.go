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
		_ = os.MkdirAll(filepath.Join(s.GitDir, "agentium-extra"), 0o700)
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

// projectTracked lists (NUL-separated) the files the project's own git
// repository tracks under the root, or "" outside one.
func (s *Store) projectTracked(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", s.Root, "ls-files", "-z", "--cached")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// maxFile is the largest file a snapshot holds.
const maxFile = 20 << 20

// largeChanged lists the new or modified files (relative, slash-separated)
// larger than maxFile.
func (s *Store) largeChanged(ctx context.Context) []string {
	out, err := s.gitRaw(ctx, "ls-files", "-z", "--others", "--modified", "--exclude-standard")
	if err != nil {
		return nil
	}
	var big []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" && tooBig(filepath.Join(s.Root, filepath.FromSlash(p))) {
			big = append(big, p)
		}
	}
	return big
}

// smallOnly drops the files larger than maxFile from a NUL-separated list.
func (s *Store) smallOnly(list string) string {
	var b strings.Builder
	for _, p := range strings.Split(list, "\x00") {
		if p != "" && !tooBig(filepath.Join(s.Root, filepath.FromSlash(p))) {
			b.WriteString(p + "\x00")
		}
	}
	return b.String()
}

func tooBig(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > maxFile
}

// gitIn is git with stdin.
func (s *Store) gitIn(ctx context.Context, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	full := append([]string{"--git-dir=" + s.GitDir, "--work-tree=" + s.Root}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = s.Root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// KeepOriginal records path's content as it was before the turn of
// checkpoint id changed it, when the snapshot did not hold it (a file
// .gitignore excludes): undo of that turn puts it back too.
func (s *Store) KeepOriginal(id, path string) {
	rel, err := filepath.Rel(s.Root, path)
	if err != nil || strings.HasPrefix(rel, "..") || id == "" {
		return
	}
	rel = filepath.ToSlash(rel)
	ctx := context.Background()
	defer s.lock()()
	if _, err := s.git(ctx, "cat-file", "-e", id+":"+rel); err == nil {
		return // in the snapshot already
	}
	if _, err := s.git(ctx, "check-ignore", "-q", "--", rel); err != nil {
		return // not ignored: the end-of-turn snapshot holds it
	}
	extras := s.extras(id)
	if _, seen := extras[rel]; seen {
		return // the first version this turn is the one to keep
	}
	blob := "-" // did not exist
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		if st.Size() > maxFile {
			return
		}
		b, err := s.git(ctx, "hash-object", "-w", "--", path)
		if err != nil {
			return
		}
		blob = b
	}
	_ = os.MkdirAll(filepath.Dir(s.extrasPath(id)), 0o700)
	f, err := os.OpenFile(s.extrasPath(id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s\t%s\n", blob, rel)
	f.Close()
}

func (s *Store) extrasPath(id string) string {
	return filepath.Join(s.GitDir, "agentium-extra", id)
}

// extras are the files kept for checkpoint id: path → blob ("-": absent).
func (s *Store) extras(id string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(s.extrasPath(id))
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		if blob, rel, ok := strings.Cut(l, "\t"); ok {
			out[rel] = blob
		}
	}
	return out
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
	// Files over maxFile (datasets, model weights, disk images) are left
	// out: copying them on every change would stall turns and fill the disk.
	spec := "."
	for _, p := range s.largeChanged(ctx) {
		spec += "\x00:(exclude,literal)" + p
	}
	if _, err := s.gitIn(ctx, spec, "add", "-A", "--ignore-errors", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return "", err
	}
	// Files the project's own git tracks are covered even where the
	// snapshot's exclusions (build/, dist/ …) would skip them.
	if tracked := s.smallOnly(s.projectTracked(ctx)); len(tracked) > 0 {
		_, _ = s.gitIn(ctx, tracked, "add", "-f", "--ignore-errors", "--pathspec-from-file=-", "--pathspec-file-nul")
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
	// Files the snapshot did not hold, kept as the turn first changed them.
	for rel, blob := range s.extras(id) {
		p := filepath.Join(s.Root, filepath.FromSlash(rel))
		touched = append(touched, rel)
		if blob == "-" {
			_ = os.Remove(p)
			continue
		}
		content, err := s.gitRaw(ctx, "cat-file", "blob", blob)
		if err != nil {
			continue
		}
		mode := os.FileMode(0o644)
		if st, err := os.Stat(p); err == nil {
			mode = st.Mode().Perm()
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = fsx.WriteFile(p, []byte(content), mode)
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
