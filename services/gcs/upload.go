package gcs

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Uploads (FR-GCS-002): simple (uploadType=media), multipart and resumable
// with chunk resume, streamed to disk (FR-GCS-010) with MD5/CRC32C computed
// and validated (FR-GCS-004).

// uploadRec is a persisted resumable upload session.
type uploadRec struct {
	ID      string          `json:"id"`
	Bucket  string          `json:"bucket"`
	Meta    *storage.Object `json:"meta"`
	Conds   conds           `json:"conds"`
	Created string          `json:"created"`
	// Total is the declared size (X-Upload-Content-Length) or -1.
	Total int64  `json:"total"`
	Base  string `json:"base,omitempty"`
	// Result is the finished object, kept so a retried final request gets
	// the same answer.
	Result *storage.Object `json:"result,omitempty"`
}

// session is the in-memory state of a resumable upload: a lock serialising
// its chunks and the running checksums of the bytes received so far.
type session struct {
	mu    sync.Mutex
	md5   hash.Hash
	crc   uint32
	n     int64
	valid bool
}

func (s *Service) session(id string, fresh bool) *session {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		sess = &session{md5: md5.New(), valid: fresh}
		s.sessions[id] = sess
	}
	return sess
}

func (s *Service) dropSession(id string) {
	s.sessMu.Lock()
	delete(s.sessions, id)
	s.sessMu.Unlock()
	if s.blobs != nil {
		_ = os.Remove(s.uploadPath(id))
	}
}

func (s *Service) uploadPath(id string) string {
	return filepath.Join(s.blobs.uploadsDir(), id)
}

func (s *Service) serveUpload(w http.ResponseWriter, r *http.Request, rest string) {
	esc := strings.Split(strings.Trim(rest, "/"), "/")
	if len(esc) != 3 || esc[0] != "b" || esc[2] != "o" {
		methodNotAllowed(w, r)
		return
	}
	bucket, err := url.PathUnescape(esc[1])
	if err != nil {
		apierr.Write(w, errInvalid("Invalid bucket name."))
		return
	}
	q := r.URL.Query()
	if id := q.Get("upload_id"); id != "" {
		switch r.Method {
		case http.MethodPut, http.MethodPost:
			s.uploadChunk(w, r, id)
		case http.MethodDelete:
			s.cancelUpload(w, r, id)
		default:
			methodNotAllowed(w, r)
		}
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r)
		return
	}
	switch q.Get("uploadType") {
	case "media", "":
		s.simpleUpload(w, r, bucket)
	case "multipart":
		s.multipartUpload(w, r, bucket)
	case "resumable":
		s.startResumable(w, r, bucket)
	default:
		apierr.Write(w, errInvalid("Invalid uploadType: %s", q.Get("uploadType")))
	}
}

// checkCreate authorizes an object write and validates the target.
func (s *Service) checkCreate(ctx context.Context, bucket, name string) error {
	if err := s.check(ctx, "storage.objects.create", bucketResource(bucket)); err != nil {
		return err
	}
	if name == "" {
		return errRequired("name")
	}
	if !validObjectName(name) {
		return errInvalid("The specified object name is not valid.")
	}
	return nil
}

// insertObjectMedia handles objects.insert on the metadata path: the body
// is object metadata and the object is empty.
func (s *Service) insertObjectMedia(w http.ResponseWriter, r *http.Request, bucket string) {
	var meta storage.Object
	if err := decodeJSON(r, &meta); err != nil {
		apierr.Write(w, err)
		return
	}
	if meta.Name == "" {
		meta.Name = r.URL.Query().Get("name")
	}
	s.writeObject(w, r, bucket, &meta, strings.NewReader(""), r.Header.Get("X-Goog-Hash"))
}

func (s *Service) simpleUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	meta := &storage.Object{
		Name:            q.Get("name"),
		ContentType:     r.Header.Get("Content-Type"),
		ContentEncoding: q.Get("contentEncoding"),
	}
	s.writeObject(w, r, bucket, meta, r.Body, r.Header.Get("X-Goog-Hash"))
}

func (s *Service) multipartUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		apierr.Write(w, errInvalid("Multipart upload requires a multipart/related body."))
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	metaPart, err := mr.NextPart()
	if err != nil {
		apierr.Write(w, errInvalid("Missing metadata part: %v", err))
		return
	}
	var meta storage.Object
	if err := json.NewDecoder(io.LimitReader(metaPart, 16<<20)).Decode(&meta); err != nil && !errors.Is(err, io.EOF) {
		apierr.Write(w, errInvalid("Invalid JSON payload received. %v", err))
		return
	}
	media, err := mr.NextPart()
	if err != nil {
		apierr.Write(w, errInvalid("Missing media part: %v", err))
		return
	}
	if meta.Name == "" {
		meta.Name = r.URL.Query().Get("name")
	}
	if meta.ContentType == "" {
		meta.ContentType = media.Header.Get("Content-Type")
	}
	s.writeObject(w, r, bucket, &meta, media, r.Header.Get("X-Goog-Hash"))
}

// writeObject streams body into a new generation of meta.Name.
func (s *Service) writeObject(w http.ResponseWriter, r *http.Request, bucket string, meta *storage.Object, body io.Reader, xGoogHash string) {
	rec, bkt, err := s.putObject(r.Context(), bucket, meta, body, xGoogHash, r.URL.Query(), baseURL(r))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderObject(rec, bkt, baseURL(r)))
}

// putObject validates, stores and commits an object from a stream.
func (s *Service) putObject(ctx context.Context, bucket string, meta *storage.Object, body io.Reader, xGoogHash string, q url.Values, base string) (*objectRec, *storage.Bucket, error) {
	meta.Name = strings.TrimSpace(meta.Name)
	if err := s.checkCreate(ctx, bucket, meta.Name); err != nil {
		return nil, nil, err
	}
	c, err := parseConds(q, "")
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.getBucketRec(bucket); err != nil {
		return nil, nil, err
	}
	bw, err := s.blobs.newWriter()
	if err != nil {
		return nil, nil, err
	}
	if _, err := bw.ReadFrom(body); err != nil {
		bw.abort()
		return nil, nil, errInvalid("Failed to read upload body: %v", err)
	}
	info, err := bw.commit()
	if err != nil {
		return nil, nil, err
	}
	if err := validateHashes(meta, xGoogHash, info); err != nil {
		s.blobs.remove(info.ID)
		return nil, nil, err
	}
	return s.commitObject(ctx, &writeReq{bucket: bucket, meta: meta, blob: info, conds: c, base: base})
}

// validateHashes checks client-supplied MD5/CRC32C (JSON fields or an
// X-Goog-Hash header) against the computed ones (FR-GCS-004).
func validateHashes(meta *storage.Object, xGoogHash string, info *blobInfo) error {
	wantMD5, wantCRC := meta.Md5Hash, meta.Crc32c
	for _, part := range strings.Split(xGoogHash, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "md5":
			wantMD5 = v
		case "crc32c":
			wantCRC = v
		}
	}
	if wantCRC != "" && wantCRC != info.crcB64() {
		return errInvalid("Provided CRC32C %q doesn't match calculated CRC32C %q.", wantCRC, info.crcB64())
	}
	if wantMD5 != "" && wantMD5 != info.md5B64() {
		return errInvalid("Provided MD5 hash %q doesn't match calculated MD5 hash %q.", wantMD5, info.md5B64())
	}
	return nil
}

func newUploadID() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "ADPyc" + base64.RawURLEncoding.EncodeToString(b[:]) + hex.EncodeToString(b[:4])
}

func (s *Service) startResumable(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	var meta storage.Object
	if err := decodeJSON(r, &meta); err != nil {
		apierr.Write(w, err)
		return
	}
	if meta.Name == "" {
		meta.Name = q.Get("name")
	}
	if meta.ContentType == "" {
		meta.ContentType = r.Header.Get("X-Upload-Content-Type")
	}
	if meta.ContentEncoding == "" {
		meta.ContentEncoding = q.Get("contentEncoding")
	}
	total := int64(-1)
	if v := r.Header.Get("X-Upload-Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			total = n
		}
	}
	rec, err := s.beginResumable(r.Context(), bucket, &meta, q, total, baseURL(r))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	loc := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=resumable&name=%s&upload_id=%s",
		baseURL(r), url.PathEscape(bucket), url.QueryEscape(meta.Name), rec.ID)
	w.Header().Set("Location", loc)
	w.Header().Set("X-GUploader-UploadID", rec.ID)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// beginResumable validates the target and persists a new upload session.
func (s *Service) beginResumable(ctx context.Context, bucket string, meta *storage.Object, q url.Values, total int64, base string) (*uploadRec, error) {
	if err := s.checkCreate(ctx, bucket, meta.Name); err != nil {
		return nil, err
	}
	c, err := parseConds(q, "")
	if err != nil {
		return nil, err
	}
	var cur *objectRec
	err = s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		cur, _, err = loadObject(tx, bucket, meta.Name, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	var curObj *storage.Object
	if cur != nil {
		curObj = cur.Object
	}
	if err := c.check(curObj, false); err != nil {
		return nil, err
	}
	rec := &uploadRec{ID: newUploadID(), Bucket: bucket, Meta: meta, Conds: c, Created: ts(s.now()), Total: total, Base: base}
	f, err := os.OpenFile(s.uploadPath(rec.ID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	s.session(rec.ID, true)
	if err := s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsUploads, rec.ID, rec) }); err != nil {
		s.dropSession(rec.ID)
		return nil, err
	}
	return rec, nil
}

// contentRange is a parsed "Content-Range: bytes a-b/total" header.
type contentRange struct {
	status     bool  // "bytes */..." (no data)
	first      int64 // first byte offset
	last       int64
	total      int64 // -1 when "*"
	hasRange   bool
	wholeShape bool // no Content-Range: the body is the whole object
}

func parseContentRange(v string) (contentRange, error) {
	cr := contentRange{total: -1}
	if v == "" {
		cr.wholeShape = true
		return cr, nil
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !ok {
		return cr, errInvalid("Invalid Content-Range header: %s", v)
	}
	rng, tot, ok := strings.Cut(spec, "/")
	if !ok {
		return cr, errInvalid("Invalid Content-Range header: %s", v)
	}
	if tot != "*" {
		n, err := strconv.ParseInt(tot, 10, 64)
		if err != nil || n < 0 {
			return cr, errInvalid("Invalid Content-Range header: %s", v)
		}
		cr.total = n
	}
	if rng == "*" {
		cr.status = true
		return cr, nil
	}
	a, b, ok := strings.Cut(rng, "-")
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if !ok || err1 != nil || err2 != nil || first < 0 || last < first {
		return cr, errInvalid("Invalid Content-Range header: %s", v)
	}
	cr.first, cr.last, cr.hasRange = first, last, true
	return cr, nil
}

func (s *Service) loadUpload(id string) (*uploadRec, error) {
	var rec uploadRec
	err := s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsUploads, id, &rec) })
	if err == store.ErrNotFound {
		return nil, apierr.NotFound("No such upload session: %s", id).WithLegacy("notFound")
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// uploadChunk handles one PUT/POST of a resumable session: a data chunk,
// a status query ("bytes */*") or the final request.
func (s *Service) uploadChunk(w http.ResponseWriter, r *http.Request, id string) {
	sess := s.session(id, false)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	rec, err := s.loadUpload(id)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if rec.Result != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		bkt, _ := s.getBucketRec(rec.Bucket)
		var b *storage.Bucket
		if bkt != nil {
			b = bkt.Bucket
		}
		writeJSON(w, http.StatusOK, renderObject(&objectRec{Object: rec.Result}, b, baseURL(r)))
		return
	}
	cr, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	path := s.uploadPath(id)
	st, err := os.Stat(path)
	if err != nil {
		apierr.Write(w, apierr.NotFound("No such upload session: %s", id))
		return
	}
	size := st.Size()
	switch {
	case cr.status:
		if cr.total >= 0 && cr.total == size {
			s.finishUpload(w, r, rec, sess, size)
			return
		}
		resumeIncomplete(w, r, size)
		return
	case cr.wholeShape:
		cr.first = 0
	}
	if cr.first > size {
		apierr.Write(w, errInvalid("Invalid request. According to the Content-Range header, the upload offset is %d byte(s), which exceeds already uploaded size of %d byte(s).", cr.first, size))
		return
	}
	// Skip bytes the server already has (a retried chunk), then append.
	if skip := size - cr.first; skip > 0 {
		if _, err := io.CopyN(io.Discard, r.Body, skip); err != nil {
			resumeIncomplete(w, r, size)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	hashing := sess.valid && sess.n == size
	var dst io.Writer = f
	if hashing {
		dst = writerFunc(func(p []byte) (int, error) {
			n, err := f.Write(p)
			sess.md5.Write(p[:n])
			sess.crc = crc32.Update(sess.crc, crcTable, p[:n])
			sess.n += int64(n)
			return n, err
		})
	} else {
		sess.valid = false
	}
	bp := copyBufPool.Get().(*[]byte)
	var limit io.Reader = r.Body
	if cr.hasRange {
		limit = io.LimitReader(r.Body, cr.last+1-max(size, cr.first))
	}
	_, cerr := io.CopyBuffer(writerOnly{dst}, limit, *bp)
	copyBufPool.Put(bp)
	serr := s.blobs.sync(f)
	_ = f.Close()
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	if cerr != nil || serr != nil {
		resumeIncomplete(w, r, size)
		return
	}
	final := cr.wholeShape || (cr.total >= 0 && size == cr.total)
	if !final {
		resumeIncomplete(w, r, size)
		return
	}
	s.finishUpload(w, r, rec, sess, size)
}

// resumeIncomplete answers "308 Resume Incomplete" (or 200 with the
// X-Http-Status-Code-Override header when the client asked for no 308).
func resumeIncomplete(w http.ResponseWriter, r *http.Request, size int64) {
	if size > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", size-1))
	}
	w.Header().Set("Content-Length", "0")
	if strings.EqualFold(r.Header.Get("X-GUploader-No-308"), "yes") {
		w.Header().Set("X-Http-Status-Code-Override", "308")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusPermanentRedirect)
}

// finishUpload commits a completed resumable upload.
func (s *Service) finishUpload(w http.ResponseWriter, r *http.Request, rec *uploadRec, sess *session, size int64) {
	var sums *blobInfo
	if sess.valid && sess.n == size {
		sums = &blobInfo{Size: size, MD5: sess.md5.Sum(nil), CRC: sess.crc}
	}
	info, err := s.blobs.adoptFile(s.uploadPath(rec.ID), sums)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	s.sessMu.Lock()
	delete(s.sessions, rec.ID)
	s.sessMu.Unlock()
	if err := validateHashes(rec.Meta, r.Header.Get("X-Goog-Hash"), info); err != nil {
		s.blobs.remove(info.ID)
		_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsUploads, rec.ID) })
		apierr.Write(w, err)
		return
	}
	meta := *rec.Meta
	obj, bkt, err := s.commitObject(r.Context(), &writeReq{bucket: rec.Bucket, meta: &meta, blob: info, conds: rec.Conds, base: rec.Base})
	if err != nil {
		_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsUploads, rec.ID) })
		apierr.Write(w, err)
		return
	}
	rec.Result = obj.Object
	_ = s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsUploads, rec.ID, rec) })
	writeJSON(w, http.StatusOK, renderObject(obj, bkt, baseURL(r)))
}

// cancelUpload deletes a resumable session; GCS answers 499.
func (s *Service) cancelUpload(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := s.loadUpload(id); err != nil {
		apierr.Write(w, err)
		return
	}
	_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsUploads, id) })
	s.dropSession(id)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(499)
}
