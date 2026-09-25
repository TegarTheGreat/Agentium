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
	"path/filepath"
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
		"Run a shell command in the workspace; output clipped head+tail. Writes outside the workspace and all outbound connections (including to localhost) are blocked unless net=true (installs, downloads, git push, curl to a local server); servers may listen without it. background=true for servers/watchers/REPLs returns a job id: {job} reads new output (waits up to timeout s), {job,stdin} sends input (control chars ok, e.g. \\u0003), {job,kill} stops; {} lists jobs. tty=true gives the job a terminal (REPLs, prompts, ssh). Give tests their own time limit (go test -timeout 60s, pytest --timeout 60) so a hang fails fast with a trace.",
		`{"type":"object","properties":{"cmd":{"type":"string"},"timeout":{"type":"integer","description":"seconds, default 120"},"net":{"type":"boolean"},"background":{"type":"boolean"},"tty":{"type":"boolean"},"job":{"type":"integer"},"stdin":{"type":"string"},"kill":{"type":"boolean"}}}`),
	Run: func(ctx context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Cmd        string          `json:"cmd"`
			Command    string          `json:"command"` // what many models call it
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
		if a.Cmd == "" {
			a.Cmd = a.Command // not "(no background jobs)" for a misnamed key
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
				for _, r := range env.roots() {
					cfg.Write = sandbox.ReadOnly(cfg.Write, r)
				}
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
			guard = snapGit(env.roots()...)
		}
		out, err := runShell(env.withDetach(ctx, a.Cmd), env.Root, a.Cmd, time.Duration(t)*time.Second, box, env.PassEnv)
		out += late + guard.check()
		if guarded {
			env.mu.Lock()
			env.gitg = snapGit(env.roots()...)
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

// quietEnv keeps commands from stopping to wait for a person: pagers,
// editors and prompts either pass through or fail at once.
var quietEnv = []string{"PAGER=cat", "GIT_PAGER=cat", "GIT_EDITOR=true", "GIT_TERMINAL_PROMPT=0",
	"DEBIAN_FRONTEND=noninteractive", "PYTHONUNBUFFERED=1", "PIP_NO_INPUT=1", "CI=1"}

var spillSweep sync.Once

// clipOrSpill clips long output to its head and tail, and keeps the whole
// output in a file the model can grep instead of running the command again.
func clipOrSpill(out string) string {
	if len(out) <= bashMaxOutput {
		return out
	}
	s := Clip(out, bashMaxOutput)
	dir := filepath.Join(os.TempDir(), "agentium-output")
	spillSweep.Do(func() { // yesterday's spills are not needed any more
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	})
	if os.MkdirAll(dir, 0o700) == nil {
		if f, err := os.CreateTemp(dir, "out-*.log"); err == nil {
			f.WriteString(out)
			f.Close()
			s = strings.TrimRight(s, "\n") + fmt.Sprintf("\n[the full output (%d lines) is in %s: grep it or read parts with the read tool instead of re-running]", strings.Count(strings.TrimRight(out, "\n"), "\n")+1, f.Name())
		}
	}
	return s
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
	cmd.Env = append(policy.ScrubEnv(cmd.Env, passEnv), quietEnv...)
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
	sw := &switchWriter{w: w} // a detached command's output goes to its job
	cmd.Stdout = sw
	cmd.Stderr = sw
	dt := detachFrom(ctx)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	contain(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	timedOut := false
	var detach <-chan struct{}
	if dt != nil {
		detach = dt.arm()
		defer dt.disarm()
	}
	select {
	case err = <-done:
	case <-detach:
		// The user sent it to the background (Ctrl-B): it becomes a job
		// and keeps running; the turn goes on. (If it has just finished,
		// it is reported as usual.)
		select {
		case err = <-done:
		default:
			return dt.adopt(cmd, sw, &out, done), nil
		}
	case <-ctx.Done():
		killProcessGroup(cmd)
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			// Something keeps the output pipe open; do not hang the turn.
			err = errors.New("process did not exit after being killed")
		}
		timedOut = ctx.Err() == context.DeadlineExceeded
	}
	// Anything the command left running in its process group (a server
	// started with "&") is stopped: long-running processes belong in
	// background jobs, which are tracked and stopped at the end.
	killProcessGroup(cmd)
	s := clipOrSpill(out.String())
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
		s += "\n[processes left running were stopped; start servers with background=true]"
	}
	switch {
	case timedOut:
		// A command that hangs is usually stuck (a deadlock, a prompt
		// waiting for input, a server): a longer timeout rarely helps.
		return s, fmt.Errorf("timed out after %s; it may be stuck (deadlock, waiting for input, a server): "+
			"rather than a longer timeout, rerun with the tool's own time limit to see where it hangs "+
			"(go test -timeout 20s, pytest --timeout 20, timeout 30 <cmd>), or run servers with background=true", timeout)
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
