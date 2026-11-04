//go:build unix

package instance

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ProcessAlive reports whether a process with pid exists.
func ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	// Another user's process exists but cannot be signalled.
	return err == nil || errors.Is(err, syscall.EPERM)
}

// PIDNamespace names the space this process's pids belong to: another
// process can watch a pid only from the same one. Linux has one per
// container; every other OS has one, its own.
func PIDNamespace() string {
	if runtime.GOOS != "linux" {
		return runtime.GOOS
	}
	ns, _ := os.Readlink("/proc/self/ns/pid")
	return ns
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
