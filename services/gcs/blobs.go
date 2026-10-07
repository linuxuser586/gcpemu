package gcs

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Object data lives in immutable blob files under <service dir>/blobs
// (FR-GCS-002, FR-GCS-010). A blob is written to a temp file, fsynced and
// renamed into place before the metadata that references it is committed
// (NFR-REL-001). Each object generation owns exactly one blob; copies use
// hard links so deleting one generation never affects another.

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// copyBufPool holds the large buffers used to stream object data.
var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// blobStore manages blob files.
type blobStore struct {
	dir string // <service dir>
	// noSync skips fsync for ephemeral instances, whose data is deleted on
	// exit anyway; --data-dir instances sync every acknowledged write
	// (NFR-REL-001).
	noSync bool
}

// sync flushes f to disk unless the instance is ephemeral.
func (b *blobStore) sync(f *os.File) error {
	if b.noSync {
		return nil
	}
	return f.Sync()
}

func (b *blobStore) blobsDir() string   { return filepath.Join(b.dir, "blobs") }
func (b *blobStore) tmpDir() string     { return filepath.Join(b.dir, "tmp") }
func (b *blobStore) uploadsDir() string { return filepath.Join(b.dir, "uploads") }

func (b *blobStore) init() error {
	for _, d := range []string{b.blobsDir(), b.tmpDir(), b.uploadsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// Leftover temp files are from interrupted writes; they were never committed.
	ents, _ := os.ReadDir(b.tmpDir())
	for _, e := range ents {
		_ = os.Remove(filepath.Join(b.tmpDir(), e.Name()))
	}
	return nil
}

// reset deletes every blob, temp file and upload session file.
func (b *blobStore) reset() error {
	var errs []error
	for _, d := range []string{b.blobsDir(), b.tmpDir(), b.uploadsDir()} {
		errs = append(errs, os.RemoveAll(d))
	}
	errs = append(errs, b.init())
	return errors.Join(errs...)
}

// path returns the file path of a blob ID.
func (b *blobStore) path(id string) string {
	if len(id) < 2 {
		return filepath.Join(b.blobsDir(), id)
	}
	return filepath.Join(b.blobsDir(), id[:2], id)
}

// open opens a blob for reading.
func (b *blobStore) open(id string) (*os.File, error) { return os.Open(b.path(id)) }

// remove deletes a blob, ignoring a missing file.
func (b *blobStore) remove(id string) {
	if id == "" {
		return
	}
	_ = os.Remove(b.path(id))
}

// link creates a new blob sharing src's content (hard link, or copy when
// links are unsupported) and returns its ID.
func (b *blobStore) link(src string) (string, error) {
	id := newBlobID()
	dst := b.path(id)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Link(b.path(src), dst); err == nil {
		return id, nil
	}
	in, err := b.open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	w, err := b.newWriter()
	if err != nil {
		return "", err
	}
	if _, err := w.ReadFrom(in); err != nil {
		w.abort()
		return "", err
	}
	info, err := w.commit()
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

func newBlobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// blobInfo describes a committed blob.
type blobInfo struct {
	ID   string
	Size int64
	MD5  []byte
	CRC  uint32
}

func (i *blobInfo) md5B64() string { return base64.StdEncoding.EncodeToString(i.MD5) }
func (i *blobInfo) crcB64() string { return encodeCRC(i.CRC) }

func encodeCRC(c uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], c)
	return base64.StdEncoding.EncodeToString(b[:])
}

// blobWriter streams data into a temp file while computing MD5 and CRC32C.
type blobWriter struct {
	bs  *blobStore
	f   *os.File
	md5 hash.Hash
	crc uint32
	n   int64
}

func (b *blobStore) newWriter() (*blobWriter, error) {
	f, err := os.CreateTemp(b.tmpDir(), "w-")
	if err != nil {
		return nil, err
	}
	return &blobWriter{bs: b, f: f, md5: md5.New()}, nil
}

func (w *blobWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.md5.Write(p[:n])
	w.crc = crc32.Update(w.crc, crcTable, p[:n])
	w.n += int64(n)
	return n, err
}

// ReadFrom copies r into the blob with a large buffer.
func (w *blobWriter) ReadFrom(r io.Reader) (int64, error) {
	bp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bp)
	return io.CopyBuffer(writerOnly{w}, r, *bp)
}

type writerOnly struct{ io.Writer }

// commit fsyncs and renames the temp file into the blob directory.
func (w *blobWriter) commit() (*blobInfo, error) {
	if err := w.bs.sync(w.f); err != nil {
		w.abort()
		return nil, err
	}
	if err := w.f.Close(); err != nil {
		_ = os.Remove(w.f.Name())
		return nil, err
	}
	id := newBlobID()
	dst := w.bs.path(id)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		_ = os.Remove(w.f.Name())
		return nil, err
	}
	if err := os.Rename(w.f.Name(), dst); err != nil {
		_ = os.Remove(w.f.Name())
		return nil, err
	}
	return &blobInfo{ID: id, Size: w.n, MD5: w.md5.Sum(nil), CRC: w.crc}, nil
}

// abort discards the temp file.
func (w *blobWriter) abort() {
	_ = w.f.Close()
	_ = os.Remove(w.f.Name())
}

// adoptFile moves an already-written file (a finished resumable upload) into
// the blob directory, hashing it unless sums are supplied.
func (b *blobStore) adoptFile(path string, sums *blobInfo) (*blobInfo, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	info := sums
	if info == nil {
		h := md5.New()
		var crc uint32
		bp := copyBufPool.Get().(*[]byte)
		n, err := io.CopyBuffer(writerOnly{writerFunc(func(p []byte) (int, error) {
			h.Write(p)
			crc = crc32.Update(crc, crcTable, p)
			return len(p), nil
		})}, f, *bp)
		copyBufPool.Put(bp)
		if err != nil {
			f.Close()
			return nil, err
		}
		info = &blobInfo{Size: n, MD5: h.Sum(nil), CRC: crc}
	}
	if err := b.sync(f); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	id := newBlobID()
	dst := b.path(id)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, err
	}
	if err := os.Rename(path, dst); err != nil {
		return nil, fmt.Errorf("adopt upload: %w", err)
	}
	out := *info
	out.ID = id
	return &out, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
