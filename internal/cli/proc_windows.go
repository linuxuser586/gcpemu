package cli

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachAttrs are the process attributes `start --detach` tries in turn:
// no console, so closing the shell's window does not end the instance, and
// out of the shell's job object where the job allows it (some terminals
// kill their job when they close).
func detachAttrs() []*syscall.SysProcAttr {
	const flags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
	return []*syscall.SysProcAttr{
		{CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB, HideWindow: true},
		{CreationFlags: flags, HideWindow: true},
	}
}

// freeGiB returns the free space, in GiB, available to the user on the
// volume holding dir.
func freeGiB(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail >> 30, nil
}

// exitStatus is the exit code of a command that `gcpemu run` ran.
func exitStatus(ps *os.ProcessState) int { return ps.ExitCode() }

// groupGetsCtrlC reports whether a Ctrl+C reaches the commands that
// `gcpemu run` runs without it: the console sends it to every process
// attached to it.
func groupGetsCtrlC() bool { return true }

// elevated reports whether the process may change the host's resolver
// itself: its token is elevated (an administrator's, past UAC).
func elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
