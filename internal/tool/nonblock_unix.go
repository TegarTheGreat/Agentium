//go:build !windows

package tool

import "syscall"

const nonblockFlag = syscall.O_NONBLOCK
