package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/sandbox"
)

const (
	bashDefaultTimeout = 120
	bashMaxTimeout     = 600
	bashMaxOutput      = 16 * 1024
)

var bashTool = Tool{
	Def: providerDef("bash",
		"Run a shell command in the workspace. Output is clipped to head+tail. Writes outside the workspace and network are blocked unless net=true (for installs, downloads, git push).",
		`{"type":"object","properties":{"cmd":{"type":"string"},"timeout":{"type":"integer","description":"seconds, default 120"},"net":{"type":"boolean"}},"required":["cmd"]}`),
	Run: func(ctx context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Cmd     string `json:"cmd"`
			Timeout int    `json:"timeout"`
			Net     bool   `json:"net"`
		}
		if err := decode(raw, &a); err != nil {
			return "", err
		}
		if a.Cmd == "" {
			return "", errors.New("cmd is required")
		}
		if env.Gate != nil {
			if ok, why := env.Gate.Bash(a.Cmd); !ok {
				return "", fmt.Errorf("denied (%s); choose another approach or ask the user", why)
			}
		}
		var box *sandbox.Config
		if env.Sandbox != nil {
			cfg := *env.Sandbox
			if a.Net {
				ok, why := true, ""
				if env.Gate != nil {
					ok, why = env.Gate.Net(a.Cmd, env.Net)
				} else {
					ok = env.Net == policy.NetAllow
				}
				if !ok {
					return "", fmt.Errorf("network denied (%s); do it without network or ask the user", why)
				}
				cfg.Network = true
			}
			box = &cfg
		}
		env.mutate()
		t := a.Timeout
		if t <= 0 {
			t = bashDefaultTimeout
		}
		if t > bashMaxTimeout {
			t = bashMaxTimeout
		}
		out, err := runShell(ctx, env.Root, a.Cmd, time.Duration(t)*time.Second, box)
		if box != nil && err == nil && sandboxHint.MatchString(out) {
			if box.Network {
				out += "\n[sandbox: writes outside the workspace are blocked]"
			} else {
				out += "\n[sandbox: writes outside the workspace and network are blocked; retry with net=true if network is needed]"
			}
		}
		return out, err
	},
}

var sandboxHint = regexp.MustCompile(`(?i)permission denied|operation not permitted|read-only file system|network is unreachable|could not resolve|connection refused|EACCES|EPERM`)

// lockedBuffer lets stdout and stderr share one buffer safely.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Keep memory bounded on chatty commands: retain the first and the
	// latest output only.
	if l.b.Len() > 4*1024*1024 {
		keep := l.b.Bytes()[l.b.Len()-1024*1024:]
		head := append([]byte(nil), l.b.Bytes()[:256*1024]...)
		tail := append([]byte(nil), keep...)
		l.b.Reset()
		l.b.Write(head)
		l.b.WriteString("\n[... output truncated ...]\n")
		l.b.Write(tail)
	}
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func runShell(ctx context.Context, dir, cmdline string, timeout time.Duration, box *sandbox.Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(shellPath(), "-c", cmdline)
	if box != nil {
		c, _, err := sandbox.Command(shellPath(), cmdline, *box)
		if err != nil {
			return "", err
		}
		cmd = c
	}
	cmd.Dir = dir
	setProcessGroup(cmd)
	// Background children (e.g. `server &`) may keep the pipe open; don't
	// wait on them forever once the shell itself has exited.
	cmd.WaitDelay = 500 * time.Millisecond
	var out lockedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-ctx.Done():
		killProcessGroup(cmd)
		err = <-done
		timedOut = ctx.Err() == context.DeadlineExceeded
	}
	s := Clip(out.String(), bashMaxOutput)
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
		s += "\n[background process still running]"
	}
	switch {
	case timedOut:
		return s, fmt.Errorf("timed out after %s", timeout)
	case ctx.Err() != nil:
		return s, ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if s == "" {
				s = "(no output)"
			}
			return fmt.Sprintf("%s\n[exit %d]", s, ee.ExitCode()), nil
		}
		return s, err
	}
	if s == "" {
		s = "(no output, exit 0)"
	}
	return s, nil
}
