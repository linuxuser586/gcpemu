//go:build unix

package main_test

import (
	"errors"
	"syscall"
)

// alive reports whether a process with pid exists.
func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// sigterm sends SIGTERM to pid.
func sigterm(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// kill ends pid at once, with no chance to shut down.
func kill(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
