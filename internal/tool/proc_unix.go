//go:build !windows

package tool

import (
	"os"
	"os/exec"
	"syscall"
)

func shellPath() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// sharedFile reports a file an atomic rename would change in ways the
// user did not ask for: extra hard links (the rename splits them) or
// another owner (a new file would be ours).
func sharedFile(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && (sys.Nlink > 1 || int(sys.Uid) != os.Getuid())
}
