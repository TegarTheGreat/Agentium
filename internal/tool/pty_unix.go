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
