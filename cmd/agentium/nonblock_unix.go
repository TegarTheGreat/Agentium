//go:build !windows

package main

import "syscall"

const syscallNonblock = syscall.O_NONBLOCK
