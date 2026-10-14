package emu

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Snapshotter is implemented by services that take part in State
// snapshots (FR-CORE-012) beyond the Store, which the Instance saves and
// loads itself: services holding state outside it (blob files, Postgres
// data) or rebuilding in-memory state or data planes from it.
type Snapshotter interface {
	// Snapshot saves the state the Store does not hold into dir, an empty
	// directory of the service's own. The Instance is ready and no
	// Operation is running.
	Snapshot(ctx context.Context, dir string) error
	// Restore brings the service back to a State snapshot after Reset and
	// after the Store was loaded with the snapshot's contents. dir holds
	// what Snapshot saved; it may be missing for a snapshot taken without
	// this service.
	Restore(ctx context.Context, dir string) error
}

// LinkTree recreates the files under src beneath dst as hard links,
// copying them where links are unsupported (across filesystems). It is
// for files that are never modified in place, such as blobs; a missing
// src is an empty tree.
func LinkTree(src, dst string) error { return walkTree(src, dst, true) }

// CopyTree copies the files under src beneath dst; a missing src is an
// empty tree.
func CopyTree(src, dst string) error { return walkTree(src, dst, false) }

func walkTree(src, dst string, link bool) error {
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			return nil
		case link && os.Link(p, target) == nil:
			return nil
		}
		return copyFile(p, target)
	})
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(src); errors.Is(serr, fs.ErrNotExist) {
			return nil
		}
	}
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
