package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// The Instance log's limits: rotate at this size, keeping this many old
// files (gcpemu.log.1, the newest, to gcpemu.log.3), checking the size at
// this interval.
const (
	logMaxSize       = 100 << 20
	logKeep          = 3
	logCheckInterval = 5 * time.Second
)

// copyLogTail and truncateLog are rotateLog's copy and truncate (variables
// for tests).
var (
	copyLogTail = copyTail
	truncateLog = os.Truncate
)

// startLogRotation checks the size of the Instance log at path at once,
// then in the background every interval on the real clock until done
// closes, and rotates the log when it has reached maxSize. The Emulator
// clock plays no part: advancing it never rotates. The returned channel
// closes when the checks have stopped. Rotation errors are logged, never
// returned: they must not stop the Instance.
func startLogRotation(done <-chan struct{}, path string, log *slog.Logger, maxSize int64, keep int, interval time.Duration) <-chan struct{} {
	var pending *logRotation // copied, but not yet truncated
	check := func() {
		if pending == nil {
			fi, err := os.Stat(path)
			if err != nil || fi.Size() < maxSize {
				return
			}
			pending = copyOldLog(path, fi.Size(), maxSize, keep, log)
		}
		if pending.truncate(path, log) == nil {
			pending = nil
		}
	}
	check()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				check()
			}
		}
	}()
	return stopped
}

// rotateLog rotates the Instance log at path, of at least maxSize bytes:
// it moves path.1 to path.2 and so on, keeping keep old files, copies the
// log to path.1 and truncates it, logging what it did to log. path.1 holds
// at most maxSize bytes: the end of the log, from a line boundary.
//
// It copies the log and truncates it in place rather than renaming it and
// opening a new file. Writers the detached child does not control hold the
// log open in append mode: the Cloud SQL proxy and GKE node agent
// subprocesses, Go panic traces, and the handle `start --detach` opened in
// the parent. Each appends at the end of the file, so after a truncation
// they write at the start of the empty file, with no change. After a
// rename they would keep writing to the renamed, or deleted, file, whose
// space would never be freed, and renaming an open file behaves
// differently on Windows. The cost: lines written between the copy and
// the truncate are lost.
//
// A failed copy loses the earlier log: rotateLog removes the partial
// path.1, truncates anyway and logs a warning. A failed truncate is logged
// and returned; startLogRotation's next check retries only the truncate.
func rotateLog(path string, maxSize int64, keep int, log *slog.Logger) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return copyOldLog(path, fi.Size(), maxSize, keep, log).truncate(path, log)
}

// logRotation is a rotation of the Instance log that has copied it, or
// failed to, and has yet to truncate it.
type logRotation struct {
	size    int64 // the log's size when copied
	copyErr error
}

// copyOldLog shifts the old files of the log at path, of size bytes, and
// copies its end to path.1, as rotateLog does.
func copyOldLog(path string, size, maxSize int64, keep int, log *slog.Logger) *logRotation {
	// Shift first, freeing the oldest file's space before the copy.
	if err := os.Remove(oldLog(path, keep)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("rotating instance log", "err", err)
	}
	for i := keep - 1; i >= 1; i-- {
		if err := os.Rename(oldLog(path, i), oldLog(path, i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("rotating instance log", "err", err)
		}
	}
	err := copyLogTail(path, oldLog(path, 1), size, maxSize)
	if err != nil {
		_ = os.Remove(oldLog(path, 1))
	}
	return &logRotation{size: size, copyErr: err}
}

// truncate truncates the log at path and logs the rotation's outcome.
func (r *logRotation) truncate(path string, log *slog.Logger) error {
	if err := truncateLog(path, 0); err != nil {
		log.Warn("instance log not rotated: truncating it failed", "size", r.size, "err", err)
		return err
	}
	if r.copyErr != nil {
		log.Warn("earlier instance log lost: copying it failed", "size", r.size, "err", r.copyErr)
		return nil
	}
	log.Info("instance log rotated", "size", r.size)
	return nil
}

// oldLog is the name of the ith old file of the log at path.
func oldLog(path string, i int) string { return fmt.Sprintf("%s.%d", path, i) }

// copyTail copies the first size bytes of src to dst, which it creates
// with mode 0600. Of more than maxSize bytes it copies only the last
// maxSize, starting after their first newline.
func copyTail(src, dst string, size, maxSize int64) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r := io.Reader(io.LimitReader(in, size))
	if size > maxSize {
		if _, err := in.Seek(size-maxSize, io.SeekStart); err != nil {
			return err
		}
		br := bufio.NewReader(io.LimitReader(in, maxSize))
		// Skip to the first line boundary. Without one, this drains br
		// and nothing is copied.
		for {
			_, err := br.ReadSlice('\n')
			if err == nil || errors.Is(err, io.EOF) {
				break
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				return err
			}
		}
		r = br
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, r)
	return err
}

// printLogs writes the Instance log at path to w, its old files first,
// oldest first, one line at a time. Given svc, it writes only that
// Service's lines, in the text or JSON log format. Only when none of the
// files exists does it fail, as when the log itself is missing.
func printLogs(w io.Writer, path, svc string) error {
	var found bool
	for i := logKeep; i >= 0; i-- {
		name := path
		if i > 0 {
			name = oldLog(path, i)
		}
		err := printLog(w, name, svc)
		switch {
		case err == nil:
			found = true
		case errors.Is(err, os.ErrNotExist) && (i > 0 || found):
		default:
			return err
		}
	}
	return nil
}

// printLog writes the log at path to w as printLogs does.
func printLog(w io.Writer, path, svc string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		// One line in memory at a time. Unlike a Scanner's, ReadBytes's
		// lines have no length limit.
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && (svc == "" || bytes.Contains(line, []byte("service="+svc)) || bytes.Contains(line, []byte(`"service":"`+svc+`"`))) {
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
