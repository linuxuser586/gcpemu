package instance

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process
// that has not exited (STILL_ACTIVE).
const stillActive = 259

// ProcessAlive reports whether a process with pid exists.
func ProcessAlive(pid int) bool {
	h, err := openProcess(pid)
	if err != nil {
		// Another user's process exists but cannot be opened.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// PIDScope is an opaque value that says where this process's pids mean
// something: another process can watch a pid only from an equal scope.
func PIDScope() string { return "windows" }

// exePath returns the path of the executable that the process pid runs.
func exePath(pid int) (string, error) {
	h, err := openProcess(pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// openProcess opens the process pid to query it; the caller closes the
// handle.
func openProcess(pid int) (windows.Handle, error) {
	return windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
}
