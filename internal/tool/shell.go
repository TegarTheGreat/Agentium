package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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
		"Run a shell command in the workspace; output clipped head+tail. Writes outside the workspace and network are blocked unless net=true (installs, downloads, git push). background=true for servers/watchers/REPLs returns a job id: {job} reads new output (waits up to timeout s), {job,stdin} sends input (control chars ok, e.g. \\u0003), {job,kill} stops; {} lists jobs. tty=true gives the job a terminal (REPLs, prompts, ssh).",
		`{"type":"object","properties":{"cmd":{"type":"string"},"timeout":{"type":"integer","description":"seconds, default 120"},"net":{"type":"boolean"},"background":{"type":"boolean"},"tty":{"type":"boolean"},"job":{"type":"integer"},"stdin":{"type":"string"},"kill":{"type":"boolean"}}}`),
	Run: func(ctx context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Cmd        string          `json:"cmd"`
			Timeout    int             `json:"timeout"`
			Net        bool            `json:"net"`
			Background bool            `json:"background"`
			TTY        bool            `json:"tty"`
			Job        int             `json:"job"`
			Stdin      string          `json:"stdin"`
			Kill       json.RawMessage `json:"kill"`
		}
		if err := decode(raw, &a); err != nil {
			return "", err
		}
		// kill is a boolean; some models send the job id there instead.
		kill := false
		switch k := strings.TrimSpace(string(a.Kill)); {
		case k == "" || k == "false" || k == "null":
		case k == "true":
			kill = true
		default:
			n, err := strconv.Atoi(strings.Trim(k, `"`))
			if err != nil || n <= 0 || (a.Job > 0 && a.Job != n) {
				return "", errors.New("kill must be true, with the job id in job")
			}
			a.Job, kill = n, true
		}
		if a.Job > 0 {
			if a.Cmd != "" {
				return "", errors.New("give either cmd or job, not both")
			}
			return env.jobAction(ctx, a.Job, a.Stdin, kill, a.Timeout)
		}
		if a.Cmd == "" {
			if a.Stdin != "" || kill {
				return "", errors.New("stdin and kill need a job id")
			}
			return env.listJobs(), nil
		}
		plan := env.Gate != nil && env.Gate.GetMode() == policy.Plan
		switch {
		case plan && env.Sandbox != nil:
			// The sandbox makes the workspace read-only below, so any
			// non-destructive command may run.
			if why := policy.RiskyCommand(a.Cmd); why != "" {
				return "", fmt.Errorf("denied (%s; plan mode is read-only)", why)
			}
		case env.Gate != nil:
			if ok, why := env.Gate.Bash(a.Cmd); !ok {
				if plan {
					return "", fmt.Errorf("denied (%s); only read-only commands run in plan mode", why)
				}
				return "", fmt.Errorf("denied (%s); choose another approach or ask the user", why)
			}
		}
		var box *sandbox.Config
		if env.Sandbox != nil {
			cfg := *env.Sandbox
			if plan {
				cfg.Write = sandbox.ReadOnly(cfg.Write, env.Root)
			}
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
		if !plan {
			env.mutate()
		}
		if a.Background {
			return env.startJob(a.Cmd, box, a.TTY)
		}
		t := a.Timeout
		if t <= 0 {
			t = bashDefaultTimeout
		}
		if t > bashMaxTimeout {
			t = bashMaxTimeout
		}
		guarded := env.Gate == nil || env.Gate.GetMode() != policy.Yolo
		var late string
		if guarded {
			env.mu.Lock()
			prev := env.gitg
			env.mu.Unlock()
			late = prev.check() // a background process may have changed git since
		}
		var guard *gitGuard
		if guarded {
			guard = snapGit(env.Root)
		}
		out, err := runShell(ctx, env.Root, a.Cmd, time.Duration(t)*time.Second, box, env.PassEnv)
		out += late + guard.check()
		if guarded {
			env.mu.Lock()
			env.gitg = snapGit(env.Root)
			env.mu.Unlock()
		}
		if box != nil && err == nil && sandboxHint.MatchString(out) {
			if box.Network || box.NetworkUnenforced {
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

func runShell(ctx context.Context, dir, cmdline string, timeout time.Duration, box *sandbox.Config, passEnv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(shellPath(), shellArgs(shellPath(), cmdline)...)
	if box != nil {
		c, _, err := sandbox.Command(shellPath(), cmdline, *box)
		if err != nil {
			return "", err
		}
		cmd = c
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = policy.ScrubEnv(cmd.Env, passEnv)
	cmd.Dir = dir
	setProcessGroup(cmd)
	// Background children (e.g. `server &`) may keep the pipe open; don't
	// wait on them forever once the shell itself has exited.
	cmd.WaitDelay = 500 * time.Millisecond
	var out lockedBuffer
	var w io.Writer = &out
	if lw := liveFrom(ctx); lw != nil {
		w = io.MultiWriter(&out, lw)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		return "", err
	}
	contain(cmd)
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
