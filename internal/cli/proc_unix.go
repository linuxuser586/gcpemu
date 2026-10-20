//go:build unix

package cli

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// detachAttrs are the process attributes `start --detach` tries in turn:
// a new session, so the instance outlives the shell.
func detachAttrs() []*syscall.SysProcAttr {
	return []*syscall.SysProcAttr{{Setsid: true}}
}

// processAlive reports whether a process with pid exists.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// freeGiB returns the free space, in GiB, of the file system holding dir.
func freeGiB(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize) >> 30, nil
}

// exitStatus is the exit code of a command that `gcpemu run` ran: 128+n
// when signal n killed it, as a shell reports it.
func exitStatus(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// groupGetsCtrlC reports whether a Ctrl+C reaches the commands that
// `gcpemu run` runs without it: the terminal sends SIGINT to its whole
// foreground process group, which they share with this process.
func groupGetsCtrlC() bool {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer f.Close()
	pg, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPGRP)
	return err == nil && pg == unix.Getpgrp()
}
