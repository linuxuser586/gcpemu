//go:build unix

package cli

import "syscall"

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
