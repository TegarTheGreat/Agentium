package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// statusExtra is what the status line shows after the mode: the git
// branch and how many files are changed, or the first line printed by
// the user's "status_line" command (JSON about the session on stdin).
type statusExtra struct {
	text    atomic.Value // string
	running atomic.Bool
	dir     string
	command string
	session func() map[string]any
}

func (s *statusExtra) get() string {
	t, _ := s.text.Load().(string)
	return t
}

// refresh recomputes the text in the background; calls while one runs
// are dropped.
func (s *statusExtra) refresh() {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.running.Store(false)
		var t string
		if s.command != "" {
			t = s.custom()
		} else {
			t = gitStatus(s.dir)
		}
		s.text.Store(t)
		if f := activeFS(); f != nil {
			f.mu.Lock()
			f.dirty = true
			f.mu.Unlock()
		}
	}()
}

// every refreshes on a timer (files change outside agentium too).
func (s *statusExtra) every(d time.Duration) {
	go func() {
		for range time.Tick(d) {
			s.refresh()
		}
	}()
}

func (s *statusExtra) custom() string {
	var input []byte
	if s.session != nil {
		input, _ = json.Marshal(s.session())
	}
	out, err := runHook(context.Background(), s.command, s.dir, input)
	if err != nil && out == "" {
		return "status_line: " + firstLine(err.Error())
	}
	return sanitizeStatus(firstLine(out))
}

// sanitizeStatus keeps colors (SGR) and drops other control sequences.
func sanitizeStatus(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';' || s[j] == ':') {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				b.WriteString(s[i : j+1])
			}
			i = j
			continue
		}
		if c == 0x1b && i+1 < len(s) && s[i+1] == ']' { // OSC: up to BEL or ST
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
				j++
			}
			if j < len(s) && s[j] == 0x1b {
				j++
			}
			i = j
			continue
		}
		if c < 0x20 || c == 0x7f {
			continue
		}
		b.WriteByte(c)
	}
	return b.String() + "\x1b[0m"
}

// gitStatus is the branch (in color) and the number of changed files,
// "" outside a repository. No icon: branch glyphs are missing from many
// terminal fonts.
func gitStatus(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "-b", "--untracked-files=normal")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "## ") {
		return ""
	}
	head := strings.TrimPrefix(lines[0], "## ")
	branch, rest, _ := strings.Cut(head, "...")
	branch = strings.TrimPrefix(branch, "No commits yet on ")
	if strings.HasPrefix(head, "HEAD (no branch)") {
		branch = "detached"
	}
	t := sgr(cInk) + branch + "\x1b[0m"
	if i := strings.Index(rest, "[ahead "); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimRight(strings.Fields(rest[i+7:])[0], "],")); err == nil {
			t += sgr(cGray) + " ↑" + strconv.Itoa(n) + "\x1b[0m"
		}
	}
	if n := len(lines) - 1; n > 0 {
		t += sgr(cGray) + " · " + strconv.Itoa(n) + " changed\x1b[0m"
	}
	return t
}
