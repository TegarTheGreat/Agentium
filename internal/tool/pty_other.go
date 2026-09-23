//go:build !linux && !darwin

package tool

import (
	"errors"
	"os"
	"os/exec"
)

func openPTY() (master, slave *os.File, err error) {
	return nil, nil, errors.New("terminal (tty) jobs are not supported on this OS")
}

func ttyAttr(*exec.Cmd) {}
