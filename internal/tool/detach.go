package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// A foreground command can be sent to the background while it runs
// (Ctrl-B in the UI, as in Claude Code): it becomes a job the agent can
// read and stop, and the turn continues.

// switchWriter is an output destination that can be changed while the
// command writes to it.
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *switchWriter) set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

type detacher struct {
	env *Env
	cmd string
	ch  chan struct{}
}

type detachKey struct{}

// withDetach lets the command run under ctx be sent to the background.
func (e *Env) withDetach(ctx context.Context, cmdline string) context.Context {
	return context.WithValue(ctx, detachKey{}, &detacher{env: e, cmd: cmdline})
}

func detachFrom(ctx context.Context) *detacher {
	d, _ := ctx.Value(detachKey{}).(*detacher)
	return d
}

// arm marks a foreground command as running and returns the channel a
// detach request arrives on.
// Tool calls may run side by side: each running command has its own
// channel, and Ctrl-B sends all of them to the background.
func (d *detacher) arm() <-chan struct{} {
	r := d.env.root()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.detachCh == nil {
		r.detachCh = map[chan struct{}]bool{}
	}
	d.ch = make(chan struct{}, 1)
	r.detachCh[d.ch] = true
	return d.ch
}

func (d *detacher) disarm() {
	r := d.env.root()
	r.mu.Lock()
	delete(r.detachCh, d.ch)
	r.mu.Unlock()
}

// root is the session's Env (a sub-agent's has a parent).
func (e *Env) root() *Env {
	for e.parent != nil {
		e = e.parent
	}
	return e
}

// DetachForeground sends the running foreground command to the
// background; false when none is running.
func (e *Env) DetachForeground() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.detachCh {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return len(e.detachCh) > 0
}

// adopt turns the running command into a job.
func (d *detacher) adopt(cmd *exec.Cmd, sw *switchWriter, out *lockedBuffer, done <-chan error) string {
	t := d.env.jobTable()
	t.mu.Lock()
	t.next++
	id := t.next
	j := &job{id: id, cmd: d.cmd, c: cmd, done: make(chan struct{})}
	t.jobs[id] = j
	closed := t.closed
	t.mu.Unlock()
	sw.set(j) // from now on its output is the job's
	sofar := out.String()
	go func() {
		err := <-done
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil // the shell is done; what it left running was stopped
		}
		j.err = err
		close(j.done)
	}()
	if closed {
		j.kill()
	}
	until := "keeps running"
	if d.env.parent != nil {
		until = "runs until this sub-task ends"
	}
	return fmt.Sprintf("%s\n[the user moved this command to the background: it is job %d and %s. "+
		"Read new output with {job:%d} and stop it with {job:%d, kill:true}; do not start it again.]",
		clipOrSpill(sofar), id, until, id, id)
}
