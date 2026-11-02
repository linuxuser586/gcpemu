//go:build unix

package instance

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ProcessAlive reports whether a process with pid exists.
func ProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// exePath returns the path of the executable that the process pid runs.
func exePath(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
		return strings.TrimSuffix(p, " (deleted)"), err
	}
	// macOS and the BSDs report the full path as the command.
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out)), err
}
