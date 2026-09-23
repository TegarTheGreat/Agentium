package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	return note + s
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

func (j *job) status() string {
	if j.running() {
		return "running"
	}
	if ee, ok := j.err.(*exec.ExitError); ok {
		return fmt.Sprintf("exited %d", ee.ExitCode())
	}
	if j.err != nil {
		return "exited: " + j.err.Error()
	}
	return "exited 0"
}

type jobTable struct {
	mu   sync.Mutex
	next int
	jobs map[int]*job
}

func (e *Env) jobTable() *jobTable {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.jobs == nil {
		e.jobs = &jobTable{jobs: map[int]*job{}}
	}
	return e.jobs
}

// startJob runs cmdline in the background.
func (e *Env) startJob(cmdline string, box *sandbox.Config) (string, error) {
	t := e.jobTable()
	t.mu.Lock()
	live := 0
	for _, j := range t.jobs {
		if j.running() {
			live++
		}
	}
	if live >= maxJobs {
		t.mu.Unlock()
		return "", fmt.Errorf("%d background jobs are already running; stop one first (bash {job:N, kill:true})", live)
	}
	t.next++
	id := t.next
	t.mu.Unlock()

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
	setProcessGroup(cmd)
	j := &job{id: id, cmd: cmdline, c: cmd, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = j, j
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	j.stdin = in
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() {
		j.err = cmd.Wait()
		close(j.done)
	}()
	t.mu.Lock()
	t.jobs[id] = j
	t.mu.Unlock()
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
			killProcessGroup(j.c)
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
	for _, j := range t.jobs {
		if j.running() {
			killProcessGroup(j.c)
		}
	}
}

func oneLineCmd(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}
