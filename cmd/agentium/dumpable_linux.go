//go:build linux

package main

import "syscall"

// Commands run as the same user could read this process's environment
// (API keys) from /proc/<pid>/environ, or ptrace it. Marking the process
// non-dumpable makes both need root.
func init() {
	const prSetDumpable = 4
	syscall.Syscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
}
