package tool

import (
	"context"
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
func (d *detacher) arm() <-chan struct{} {
	d.env.mu.Lock()
	defer d.env.mu.Unlock()
	d.env.detachCh = make(chan struct{}, 1)
	return d.env.detachCh
}

func (d *detacher) disarm() {
	d.env.mu.Lock()
	d.env.detachCh = nil
	d.env.mu.Unlock()
}

// DetachForeground sends the running foreground command to the
// background; false when none is running.
func (e *Env) DetachForeground() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.detachCh == nil {
		return false
	}
	select {
	case e.detachCh <- struct{}{}:
	default:
	}
	return true
}

// adopt turns the running command into a job.
func (d *detacher) adopt(cmd *exec.Cmd, sw *switchWriter, sofar string, done <-chan error) string {
	t := d.env.jobTable()
	t.mu.Lock()
	t.next++
	id := t.next
	j := &job{id: id, cmd: d.cmd, c: cmd, done: make(chan struct{})}
	t.jobs[id] = j
	closed := t.closed
	t.mu.Unlock()
	sw.set(j) // from now on its output is the job's
	go func() {
		j.err = <-done
		close(j.done)
	}()
	if closed {
		j.kill()
	}
	return fmt.Sprintf("%s\n[the user moved this command to the background: it is job %d and keeps running. "+
		"Read new output with {job:%d} and stop it with {job:%d, kill:true}; do not start it again.]",
		clipOrSpill(sofar), id, id, id)
}
