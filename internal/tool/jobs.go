package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/policy"
	"github.com/tegarthegreat/agentium/internal/sandbox"
)

// Background jobs: dev servers, watchers and REPLs keep running between
// tool calls. The model starts one with bash {background:true}, then
// reads new output, sends input or stops it by job id.

const (
	maxJobs      = 8
	jobKeepBytes = 1 << 20 // recent output kept per job
)

type job struct {
	id    int
	cmd   string
	c     *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
	err   error
	tty   bool // runs in a pseudo-terminal; output is cleaned of escapes

	mu      sync.Mutex
	buf     []byte // the latest output, at most jobKeepBytes
	total   int64  // bytes ever written
	readPos int64  // bytes already shown to the model
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf = append(j.buf, p...)
	if len(j.buf) > jobKeepBytes {
		j.buf = append([]byte(nil), j.buf[len(j.buf)-jobKeepBytes:]...)
	}
	j.total += int64(len(p))
	return len(p), nil
}

// unread returns output not shown yet (noting any that was dropped).
func (j *job) unread() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	start := j.total - int64(len(j.buf)) // offset of buf[0]
	note := ""
	if j.readPos < start {
		note = fmt.Sprintf("[... %d bytes of older output dropped ...]\n", start-j.readPos)
		j.readPos = start
	}
	s := string(j.buf[j.readPos-start:])
	j.readPos = j.total
	return note + cleanTTY(s)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?<=>]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>78NOM]`)

// cleanTTY turns terminal output into plain text: escape sequences
// (colors, cursor moves, titles) removed, CRLF and bare CR resolved.
func cleanTTY(s string) string {
	s = ansi.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if k := strings.LastIndexByte(l, '\r'); k >= 0 {
			// A carriage return redraws the line (progress bars): keep
			// what was drawn last.
			lines[i] = l[k+1:]
		}
	}
	return strings.Join(lines, "\n")
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// kill stops the job and everything it started.
func (j *job) kill() {
	if j.tty {
		killSession(j.c)
	} else {
		killProcessGroup(j.c)
	}
}

func (j *job) status() string {
	if j.running() {
		return "running"
	}
	if ee, ok := j.err.(*exec.ExitError); ok {
		if ee.ExitCode() < 0 { // ended by a signal (Ctrl-C, kill)
			return "exited (" + ee.Error() + ")"
		}
		return fmt.Sprintf("exited %d", ee.ExitCode())
	}
	if j.err != nil {
		return "exited: " + j.err.Error()
	}
	return "exited 0"
}

type jobTable struct {
	mu       sync.Mutex
	next     int
	jobs     map[int]*job
	starting int  // jobs being started (they count toward the limit)
	closed   bool // KillJobs ran: the session is over
}

func (e *Env) jobTable() *jobTable {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.jobs == nil {
		e.jobs = &jobTable{jobs: map[int]*job{}}
	}
	return e.jobs
}

// startJob runs cmdline in the background, in a pseudo-terminal when tty
// is set (for programs that insist on one: REPLs, prompts, ssh).
func (e *Env) startJob(cmdline string, box *sandbox.Config, tty bool) (string, error) {
	t := e.jobTable()
	t.mu.Lock()
	live := 0
	for _, j := range t.jobs {
		if j.running() {
			live++
		}
	}
	if live+t.starting >= maxJobs {
		t.mu.Unlock()
		return "", fmt.Errorf("%d background jobs are already running; stop one first (bash {job:N, kill:true})", live)
	}
	t.next++
	id := t.next
	t.starting++ // holds the slot while the process starts
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.starting--
		t.mu.Unlock()
	}()

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
	cmd.Env = policy.ScrubEnv(cmd.Env, e.PassEnv)
	cmd.Dir = e.Root
	j := &job{id: id, cmd: cmdline, c: cmd, done: make(chan struct{}), tty: tty}
	if tty {
		master, slave, err := openPTY()
		if err != nil {
			return "", err
		}
		cmd.Env = append(cmd.Env, "TERM=xterm-256color", "COLUMNS=120", "LINES=40")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		ttyAttr(cmd)
		if err := cmd.Start(); err != nil {
			master.Close()
			slave.Close()
			return "", err
		}
		contain(cmd)
		slave.Close() // the child holds its own copy
		j.stdin = master
		copied := make(chan struct{})
		go func() { io.Copy(j, master); close(copied) }() // ends with EIO when the child exits
		go func() {
			j.err = cmd.Wait()
			select {
			case <-copied:
			case <-time.After(time.Second):
			}
			master.Close()
			close(j.done)
		}()
	} else {
		setProcessGroup(cmd)
		cmd.Stdout, cmd.Stderr = j, j
		in, err := cmd.StdinPipe()
		if err != nil {
			return "", err
		}
		j.stdin = in
		if err := cmd.Start(); err != nil {
			return "", err
		}
		contain(cmd)
		go func() {
			j.err = cmd.Wait()
			close(j.done)
		}()
	}
	t.mu.Lock()
	t.jobs[id] = j
	closed := t.closed
	t.mu.Unlock()
	if closed { // the session ended while this job was starting
		j.kill()
		return "", errors.New("the session is ending")
	}
	out := j.waitOutput(context.Background(), 2*time.Second)
	return fmt.Sprintf("job %d started (%s): %s\n%s", id, j.status(), oneLineCmd(cmdline), out), nil
}

// waitOutput waits until the job prints something (then a moment more,
// so a burst arrives whole) or exits, up to d.
func (j *job) waitOutput(ctx context.Context, d time.Duration) string {
	deadline := time.Now().Add(d)
	var seen int64 = -1
	for time.Now().Before(deadline) {
		j.mu.Lock()
		total := j.total
		j.mu.Unlock()
		if !j.running() {
			break
		}
		if total > j.readPos {
			if total == seen { // output settled
				break
			}
			seen = total
		}
		select {
		case <-ctx.Done():
			return clipJob(j.unread())
		case <-j.done:
		case <-time.After(150 * time.Millisecond):
		}
	}
	return clipJob(j.unread())
}

func clipJob(s string) string {
	if s == "" {
		return "(no new output)"
	}
	return Clip(s, bashMaxOutput)
}

// jobAction handles bash calls that name a job.
func (e *Env) jobAction(ctx context.Context, id int, stdin string, kill bool, wait int) (string, error) {
	t := e.jobTable()
	t.mu.Lock()
	j := t.jobs[id]
	t.mu.Unlock()
	if j == nil {
		return "", fmt.Errorf("no job %d (list jobs with bash {} and no cmd)", id)
	}
	if kill {
		if j.running() {
			j.kill()
			select {
			case <-j.done:
			case <-time.After(3 * time.Second):
			}
		}
		return fmt.Sprintf("job %d stopped (%s)\n%s", id, j.status(), clipJob(j.unread())), nil
	}
	if stdin != "" {
		if !j.running() {
			return "", fmt.Errorf("job %d has exited (%s); cannot send input", id, j.status())
		}
		if j.stdin == nil {
			return "", fmt.Errorf("job %d was moved to the background while running and takes no input", id)
		}
		if _, err := io.WriteString(j.stdin, stdin); err != nil {
			return "", fmt.Errorf("writing to job %d: %v", id, err)
		}
	}
	d := time.Duration(wait) * time.Second
	if wait <= 0 {
		d = 2 * time.Second
	}
	if d > 120*time.Second {
		d = 120 * time.Second
	}
	out := j.waitOutput(ctx, d)
	return fmt.Sprintf("job %d %s\n%s", id, j.status(), out), nil
}

// listJobs describes all jobs.
func (e *Env) listJobs() string {
	t := e.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.jobs) == 0 {
		return "(no background jobs)"
	}
	ids := make([]int, 0, len(t.jobs))
	for id := range t.jobs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var sb strings.Builder
	for _, id := range ids {
		j := t.jobs[id]
		fmt.Fprintf(&sb, "job %d · %s · %s\n", id, j.status(), oneLineCmd(j.cmd))
	}
	return sb.String()
}

// KillJobs stops every background job (at exit).
func (e *Env) KillJobs() {
	e.mu.Lock()
	t := e.jobs
	e.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, j := range t.jobs {
		// Also jobs whose shell exited: their children may live on in
		// the process group.
		j.kill()
	}
}

// RunningJobs lists the commands of background jobs still running.
func (e *Env) RunningJobs() []string {
	e.mu.Lock()
	t := e.jobs
	e.mu.Unlock()
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, j := range t.jobs {
		if j.running() {
			out = append(out, oneLineCmd(j.cmd))
		}
	}
	return out
}

func oneLineCmd(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}
