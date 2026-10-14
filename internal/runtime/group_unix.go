//go:build !windows

package runtime

import (
	"os"
	"syscall"
)

// fileGroup returns the group id owning p.
func fileGroup(p string) (int, bool) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Gid), true
}
