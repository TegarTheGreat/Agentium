//go:build windows

package tool

import (
	"os"
	"os/exec"
)

func shellPath() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func sharedFile(os.FileInfo) bool { return false }
