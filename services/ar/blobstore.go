package ar

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// errDigestMismatch is returned when content does not hash to the digest
// the client declared.
var errDigestMismatch = errors.New("digest mismatch")

// blobStore keeps content-addressed blobs (layers, configs and manifests)
// under <dir>/blobs/sha256/<xx>/<hex> and in-progress uploads under
// <dir>/uploads. Files are written to a temp file, fsynced and renamed into
// place before any metadata referring to them is committed (NFR-REL-001).
//
// gc serialises garbage collection against publication: writers hold it for
// reading from file write until their link is committed, and the collector
// holds it exclusively while it re-checks references and deletes files.
type blobStore struct {
	dir string
	gc  sync.RWMutex
}

func newBlobStore(dir string) (*blobStore, error) {
	for _, d := range []string{"blobs/sha256", "uploads", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &blobStore{dir: dir}, nil
}

// path returns the file path of a sha256 digest ("sha256:<hex>").
func (b *blobStore) path(digest string) string {
	h := strings.TrimPrefix(digest, "sha256:")
	if len(h) < 2 {
		h = "00" + h
	}
	return filepath.Join(b.dir, "blobs", "sha256", h[:2], h)
}

func (b *blobStore) uploadPath(id string) string { return filepath.Join(b.dir, "uploads", id) }

// has reports whether a blob file exists.
func (b *blobStore) has(digest string) bool {
	_, err := os.Stat(b.path(digest))
	return err == nil
}

// open opens a blob for reading.
func (b *blobStore) open(digest string) (*os.File, error) { return os.Open(b.path(digest)) }

// read returns a whole (small) blob.
func (b *blobStore) read(digest string) ([]byte, error) { return os.ReadFile(b.path(digest)) }

// put stores r's content, verifying it against want ("" = accept any) and
// returns its digest and size.
func (b *blobStore) put(r io.Reader, want string) (string, int64, error) {
	f, err := os.CreateTemp(filepath.Join(b.dir, "tmp"), "blob-")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	d := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if want != "" && want != d {
		return d, n, errDigestMismatch
	}
	return d, n, b.install(tmp, d)
}

// install renames a verified temp file into place.
func (b *blobStore) install(tmp, digest string) error {
	dst := b.path(digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if b.has(digest) {
		return nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	syncDir(filepath.Dir(dst))
	return nil
}

// commitUpload verifies a finished upload against want and moves it into
// the blob store.
func (b *blobStore) commitUpload(id, want string) (int64, error) {
	p := b.uploadPath(id)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if d := "sha256:" + hex.EncodeToString(h.Sum(nil)); d != want {
		return n, errDigestMismatch
	}
	return n, b.install(p, want)
}

// remove deletes a blob file.
func (b *blobStore) remove(digest string) {
	_ = os.Remove(b.path(digest))
}

// reset deletes every blob and upload.
func (b *blobStore) reset() error {
	for _, d := range []string{"blobs", "uploads", "tmp"} {
		if err := os.RemoveAll(filepath.Join(b.dir, d)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(b.dir, d), 0o700); err != nil {
			return err
		}
	}
	return os.MkdirAll(filepath.Join(b.dir, "blobs", "sha256"), 0o700)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// parseDigest validates a sha256 digest string.
func parseDigest(s string) (string, error) {
	if !digestRe.MatchString(s) {
		if strings.HasPrefix(s, "sha256:") || !strings.Contains(s, ":") {
			return "", fmt.Errorf("invalid digest %q", s)
		}
		return "", fmt.Errorf("unsupported digest algorithm in %q", s)
	}
	return s, nil
}
