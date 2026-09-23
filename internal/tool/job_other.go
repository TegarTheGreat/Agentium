//go:build !windows

package tool

import "os/exec"

// contain is a no-op: on Unix the process group is killed instead.
func contain(*exec.Cmd) {}
