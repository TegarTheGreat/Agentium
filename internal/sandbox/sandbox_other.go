//go:build !linux && !darwin

package sandbox

import (
	"errors"
	"os/exec"
)

// Probe reports what this machine supports.
func Probe() Status {
	return Status{Detail: "no sandbox on this OS: commands run unconfined"}
}

func command(shell, cmdline string, _ Config) (*exec.Cmd, bool, error) {
	return exec.Command(shell, "-c", cmdline), false, nil
}

func confineAndExec(Config, []string) error {
	return errors.New("sandbox not supported on this OS")
}
