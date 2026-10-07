package ar

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// upload is an in-progress blob upload session bound to one OCI
// repository. Its bytes live in blobStore.uploadPath(id).
type upload struct {
	mu      sync.Mutex
	id      string
	ref     repoRef
	img     string
	size    int64
	started time.Time
}

// blobsRoute serves /v2/<name>/blobs/<digest>.
func (s *Service) blobsRoute(w http.ResponseWriter, r *http.Request, t *target, digest string) {
	if _, err := parseDigest(digest); err != nil {
		writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "%v", err))
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !s.authorize(w, r, t, permDownload) {
			return
		}
		s.getBlob(w, r, t, digest)
	case http.MethodDelete:
		if !s.authorize(w, r, t, permDelete) {
			return
		}
		var ok bool
		err := s.env.Store.Update(func(tx store.Tx) error {
			if _, ok = getLink(tx, t.ref, t.img, digest); !ok {
				return nil
			}
			return deleteLink(tx, t.ref, t.img, digest)
		})
		if err != nil {
			writeRegError(w, regErr(http.StatusInternalServerError, "UNKNOWN", "%v", err))
			return
		}
		if !ok {
			writeRegError(w, regErr(http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry"))
			return
		}
		s.collect([]string{digest})
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
	default:
		writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed"))
	}
}

// getBlob serves a linked blob with Range support.
func (s *Service) getBlob(w http.ResponseWriter, r *http.Request, t *target, digest string) {
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { _, ok = getLink(tx, t.ref, t.img, digest); return nil })
	if !ok {
		writeRegError(w, regErr(http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry"))
		return
	}
	f, err := s.blobs.open(digest)
	if err != nil {
		writeRegError(w, regErr(http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry"))
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Docker-Content-Digest", digest)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Etag", `"`+digest+`"`)
	h.Set("Cache-Control", "max-age=31536000")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// uploadsRoute serves /v2/<name>/blobs/uploads/[<id>].
func (s *Service) uploadsRoute(w http.ResponseWriter, r *http.Request, t *target, id string) {
	if !s.authorize(w, r, t, permUpload) {
		return
	}
	if id == "" {
		if r.Method != http.MethodPost {
			writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed"))
			return
		}
		s.startUpload(w, r, t)
		return
	}
	s.mu.Lock()
	u := s.uploads[id]
	s.mu.Unlock()
	if u == nil || u.ref != t.ref || u.img != t.img {
		writeRegError(w, regErr(http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry"))
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		s.uploadStatus(w, t, u, http.StatusNoContent)
	case http.MethodPatch:
		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, _, ok := parseContentRange(cr)
			if !ok || start != u.size {
				w.Header().Set("Range", rangeHeader(u.size))
				writeRegError(w, regErr(http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "content range %q does not match upload offset %d", cr, u.size))
				return
			}
		}
		if err := s.appendUpload(u, r.Body); err != nil {
			writeRegError(w, regErr(http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "%v", err))
			return
		}
		s.uploadStatus(w, t, u, http.StatusAccepted)
	case http.MethodPut:
		digest := r.URL.Query().Get("digest")
		if _, err := parseDigest(digest); err != nil {
			writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "%v", err))
			return
		}
		if err := s.appendUpload(u, r.Body); err != nil {
			writeRegError(w, regErr(http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "%v", err))
			return
		}
		s.finishUpload(w, t, u, digest)
	case http.MethodDelete:
		s.dropUpload(u)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
	default:
		writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed"))
	}
}

// startUpload handles POST: cross-repository mount, single-request
// monolithic upload (?digest=) or opening an upload session.
func (s *Service) startUpload(w http.ResponseWriter, r *http.Request, t *target) {
	q := r.URL.Query()
	if mount := q.Get("mount"); mount != "" {
		if s.tryMount(w, r, t, mount, q.Get("from")) {
			return
		}
	}
	if digest := q.Get("digest"); digest != "" {
		if _, err := parseDigest(digest); err != nil {
			writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "%v", err))
			return
		}
		s.blobs.gc.RLock()
		d, n, err := s.blobs.put(r.Body, digest)
		if err == nil {
			err = s.link(t, d, n)
		}
		s.blobs.gc.RUnlock()
		if errors.Is(err, errDigestMismatch) {
			writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "provided digest did not match uploaded content"))
			return
		}
		if err != nil {
			writeRegError(w, regErr(http.StatusInternalServerError, "UNKNOWN", "%v", err))
			return
		}
		s.blobCreated(w, t, d)
		return
	}
	u := &upload{id: s.uploadID(), ref: t.ref, img: t.img, started: s.env.Clock.Now()}
	f, err := os.OpenFile(s.blobs.uploadPath(u.id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		writeRegError(w, regErr(http.StatusInternalServerError, "UNKNOWN", "%v", err))
		return
	}
	_ = f.Close()
	s.mu.Lock()
	s.uploads[u.id] = u
	s.mu.Unlock()
	s.uploadStatus(w, t, u, http.StatusAccepted)
}

// tryMount links a blob from another repository; it returns false (and
// writes nothing) when the blob is not available there.
func (s *Service) tryMount(w http.ResponseWriter, r *http.Request, t *target, digest, from string) bool {
	if _, err := parseDigest(digest); err != nil || from == "" {
		return false
	}
	src, rerr := s.resolve(r, from)
	if rerr != nil || s.env.Auth.Check(r.Context(), permDownload, src.ref.resource()) != nil {
		return false
	}
	s.blobs.gc.RLock()
	defer s.blobs.gc.RUnlock()
	var ok bool
	err := s.env.Store.Update(func(tx store.Tx) error {
		l, found := getLink(tx, src.ref, src.img, digest)
		if !found || !s.blobs.has(digest) {
			return nil
		}
		ok = true
		return putLink(tx, t.ref, t.img, digest, l.Size, s.env.Clock.Now())
	})
	if err != nil || !ok {
		return false
	}
	s.blobCreated(w, t, digest)
	return true
}

// link records a stored blob in the target repository.
func (s *Service) link(t *target, digest string, size int64) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		if _, err := getRepo(tx, t.ref); err != nil {
			return err
		}
		return putLink(tx, t.ref, t.img, digest, size, s.env.Clock.Now())
	})
}

func (s *Service) blobCreated(w http.ResponseWriter, t *target, digest string) {
	h := w.Header()
	h.Set("Location", t.path("/blobs/"+digest))
	h.Set("Docker-Content-Digest", digest)
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) uploadID() string {
	h := s.env.IDs.Hex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func rangeHeader(size int64) string {
	if size <= 0 {
		return "0-0"
	}
	return fmt.Sprintf("0-%d", size-1)
}

func (s *Service) uploadStatus(w http.ResponseWriter, t *target, u *upload, status int) {
	h := w.Header()
	h.Set("Location", t.path("/blobs/uploads/"+u.id))
	h.Set("Range", rangeHeader(u.size))
	h.Set("Docker-Upload-UUID", u.id)
	h.Set("Content-Length", "0")
	w.WriteHeader(status)
}

// parseContentRange parses "start-end" (optionally prefixed "bytes ").
func parseContentRange(s string) (int64, int64, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "bytes ")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// appendUpload appends body to the upload file (caller holds u.mu).
func (s *Service) appendUpload(u *upload, body io.Reader) error {
	f, err := os.OpenFile(s.blobs.uploadPath(u.id), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, body)
	u.size += n
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// finishUpload verifies and publishes an upload (caller holds u.mu).
func (s *Service) finishUpload(w http.ResponseWriter, t *target, u *upload, digest string) {
	s.blobs.gc.RLock()
	n, err := s.blobs.commitUpload(u.id, digest)
	if err == nil {
		err = s.link(t, digest, n)
	}
	s.blobs.gc.RUnlock()
	s.dropUpload(u)
	if errors.Is(err, errDigestMismatch) {
		writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "provided digest did not match uploaded content"))
		return
	}
	if err != nil {
		writeRegError(w, regErr(http.StatusInternalServerError, "UNKNOWN", "%v", err))
		return
	}
	s.blobCreated(w, t, digest)
}

func (s *Service) dropUpload(u *upload) {
	s.mu.Lock()
	delete(s.uploads, u.id)
	s.mu.Unlock()
	_ = os.Remove(s.blobs.uploadPath(u.id))
}
