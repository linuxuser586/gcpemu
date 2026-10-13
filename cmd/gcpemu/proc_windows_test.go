package main_test

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// alive reports whether a process with pid exists.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) != nil || code == 259 // STILL_ACTIVE
}

// sigterm fails: Windows has no SIGTERM.
func sigterm(int) error {
	return errors.ErrUnsupported
}

// kill ends pid at once, with no chance to shut down.
func kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
