//go:build linux || darwin

package tool

import (
	"os/exec"
	"syscall"
)

// ttyAttr makes the child a session leader with the pty as its
// controlling terminal (its process group is its pid, so the whole job
// can still be killed at once).
func ttyAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}

// killSession stops a tty job: every process in its session. An
// interactive shell puts each of its jobs in a process group of its own,
// so killing the job's group alone would leave those running.
func killSession(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	sid := cmd.Process.Pid // the job is a session leader (ttyAttr)
	_ = syscall.Kill(-sid, syscall.SIGKILL)
	for range 3 { // again, for anything forked meanwhile
		pids := sessionPIDs(sid)
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
