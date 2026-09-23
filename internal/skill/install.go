package skill

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SourceFile records where an installed skill came from and at which
// commit, so an install is pinned and reviewable.
const SourceFile = ".agentium-source"

// Staged is a fetched skill source waiting for review.
type Staged struct {
	Dir     string // local checkout or directory
	Source  string // what the user asked for
	Commit  string // pinned commit for git sources
	cleanup func()
}

// Close removes a temporary checkout.
func (s *Staged) Close() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

// Stage fetches src without installing anything. src is a local
// directory, a git URL, or GitHub shorthand owner/repo; "#ref" pins a
// branch, tag or commit.
func Stage(src string) (*Staged, error) {
	if fi, err := os.Stat(src); err == nil && fi.IsDir() {
		abs, _ := filepath.Abs(src)
		return &Staged{Dir: abs, Source: abs}, nil
	}
	url, ref, _ := strings.Cut(src, "#")
	if !strings.Contains(url, "://") && !strings.HasPrefix(url, "git@") {
		if strings.Count(url, "/") != 1 {
			return nil, fmt.Errorf("%s: not a directory, git URL, or owner/repo", src)
		}
		url = "https://github.com/" + url
	}
	if ref == "" {
		ref = "HEAD"
	}
	tmp, err := os.MkdirTemp("", "agentium-skill-")
	if err != nil {
		return nil, err
	}
	st := &Staged{Dir: tmp, Source: url, cleanup: func() { os.RemoveAll(tmp) }}
	for _, args := range [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", url, ref},
		{"-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"},
	} {
		if _, err := git(tmp, args...); err != nil {
			st.Close()
			return nil, err
		}
	}
	commit, err := git(tmp, "rev-parse", "HEAD")
	if err != nil {
		st.Close()
		return nil, err
	}
	st.Commit = strings.TrimSpace(commit)
	return st, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Never prompt for credentials or run repository hooks/filters.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
		}
	case <-time.After(2 * time.Minute):
		cmd.Process.Kill()
		<-done
		return "", fmt.Errorf("git %s: timed out", args[0])
	}
	return out.String(), nil
}

// Found lists the skills in a staged source: SKILL.md at the root or in
// directories up to three levels down (e.g. skills/<name>/SKILL.md).
func (s *Staged) Found() []Skill {
	var out []Skill
	_ = filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.Dir, p)
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "SKILL.md" {
			if sk, err := Parse(p); err == nil {
				if rel == "SKILL.md" && s.cleanup != nil {
					// A repository that is itself one skill: name it
					// after the repository, not the temp directory.
					if n := strings.ToLower(strings.TrimSuffix(filepath.Base(s.Source), ".git")); sk.Name == strings.ToLower(filepath.Base(s.Dir)) && ValidName(n) {
						sk.Name = n
					}
				}
				out = append(out, sk)
			}
		}
		return nil
	})
	return out
}

// Install copies a staged skill into the user skill directory. Symlinks
// and .git are skipped so nothing outside the skill is pulled in.
func Install(home string, st *Staged, sk Skill) (string, error) {
	dst := filepath.Join(UserDir(home), sk.Name)
	if _, err := os.Stat(dst); err == nil {
		return "", ErrExists
	}
	if err := os.MkdirAll(UserDir(home), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(UserDir(home), ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	src := sk.Dir()
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(tmp, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(p, filepath.Join(tmp, rel), info.Mode().Perm())
	})
	if err != nil {
		return "", err
	}
	origin := st.Source
	if st.Commit != "" {
		origin += "@" + st.Commit
	}
	if err := os.WriteFile(filepath.Join(tmp, SourceFile), []byte(origin+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Remove deletes an installed (user) skill.
func Remove(home, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	dir := filepath.Join(UserDir(home), name)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return fmt.Errorf("no installed skill %q in %s", name, UserDir(home))
	}
	return os.RemoveAll(dir)
}

// Origin returns the recorded source of an installed skill, if any.
func Origin(sk Skill) string {
	b, err := os.ReadFile(filepath.Join(sk.Dir(), SourceFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
