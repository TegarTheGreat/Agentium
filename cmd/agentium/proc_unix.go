//go:build !windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

func setPgid(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// hookCommand runs a user hook through sh; a timeout ends its whole
// process group.
func hookCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	setPgid(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	return cmd
}
