//go:build windows

package tool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// shellPath prefers Git Bash (the usual bash on Windows developer
// machines) over System32\bash.exe, which starts WSL with a different
// filesystem; without bash, PowerShell.
func shellPath() string {
	for _, p := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramW6432"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Git", "bin", "bash.exe"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("bash"); err == nil && !strings.Contains(strings.ToLower(p), `\windows\system32\`) {
		return p
	}
	for _, ps := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(ps); err == nil {
			return p
		}
	}
	return "cmd.exe"
}

// shellArgs builds the argument list that runs cmdline in shell.
func shellArgs(shell, cmdline string) []string {
	switch strings.ToLower(strings.TrimSuffix(filepath.Base(shell), ".exe")) {
	case "pwsh", "powershell":
		return []string{"-NoProfile", "-NonInteractive", "-Command", cmdline}
	case "cmd":
		return []string{"/C", cmdline}
	}
	return []string{"-c", cmdline}
}

func setProcessGroup(*exec.Cmd) {}

// killProcessGroup ends the process and its children (Kill alone leaves
// the children of a shell running).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

func sharedFile(os.FileInfo) bool { return false }
