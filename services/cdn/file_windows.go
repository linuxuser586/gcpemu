package cdn

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"
)

// shareAll lets other handles read, write, rename and delete the file while
// it is open. Windows refuses to rename or remove an open file unless every
// handle allows FILE_SHARE_DELETE, which os.Open and os.CreateTemp do not set.
const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// createSpill creates the temp file a fill spills to. It is renamed into
// the disk tier while still open.
func createSpill(dir string) (*os.File, error) {
	for range 10000 {
		name := filepath.Join(dir, "fill-"+strconv.FormatUint(uint64(rand.Uint32()), 10)+".tmp")
		f, err := createFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "createtemp", Path: name, Err: err}
		}
		return f, nil
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, "fill-*.tmp"), Err: os.ErrExist}
}

// openBody opens a cached body for reading. The entry may be evicted and
// its file removed while the handle is open.
func openBody(name string) (*os.File, error) {
	f, err := createFile(name, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return f, nil
}

func createFile(name string, access, mode uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, access, shareAll, nil, mode, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}
