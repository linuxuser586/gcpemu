package ar

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"
)

// createFetchTemp creates the temp file an upstream blob fetch writes to.
// Readers follow it while it grows and it is renamed into the blob store
// while still open, which Windows allows only when the handle was opened
// with FILE_SHARE_DELETE; os.CreateTemp does not set it.
func createFetchTemp(dir string) (*os.File, error) {
	for range 10000 {
		name := filepath.Join(dir, "fetch-"+strconv.FormatUint(uint64(rand.Uint32()), 10))
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "createtemp", Path: name, Err: err}
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, "fetch-*"), Err: os.ErrExist}
}
