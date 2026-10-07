package gcs

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	storage "google.golang.org/api/storage/v1"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Downloads (FR-GCS-002): alt=media with Range support, streamed from disk.

// downloadJSON serves alt=media on the JSON API paths.
func (s *Service) downloadJSON(w http.ResponseWriter, r *http.Request, bucket, name string) {
	if err := s.check(r.Context(), "storage.objects.get", objectResource(bucket, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	q := r.URL.Query()
	gen, err := parseGeneration(q.Get("generation"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(q, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	rec, _, err := s.readObject(bucket, name, gen, c)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.serveMedia(w, r, rec); err != nil {
		apierr.Write(w, err)
	}
}

// setObjectHeaders writes the response headers GCS sends with object data
// (also used for XML HEAD).
func setObjectHeaders(h http.Header, o *storage.Object) {
	h.Set("Content-Type", o.ContentType)
	h.Set("X-Goog-Generation", strconv.FormatInt(o.Generation, 10))
	h.Set("X-Goog-Metageneration", strconv.FormatInt(o.Metageneration, 10))
	hash := "crc32c=" + o.Crc32c
	if o.Md5Hash != "" {
		hash += ",md5=" + o.Md5Hash
	}
	h.Set("X-Goog-Hash", hash)
	h.Set("X-Goog-Stored-Content-Length", strconv.FormatUint(o.Size, 10))
	enc := o.ContentEncoding
	if enc == "" {
		enc = "identity"
	}
	h.Set("X-Goog-Stored-Content-Encoding", enc)
	h.Set("X-Goog-Storage-Class", o.StorageClass)
	h.Set("Last-Modified", parseTS(o.Updated).Format(http.TimeFormat))
	h.Set("ETag", `"`+o.Md5Hash+`"`)
	if o.Md5Hash == "" {
		h.Set("ETag", `"`+etagFor(o.Generation, o.Metageneration)+`"`)
	}
	h.Set("Accept-Ranges", "bytes")
	for k, v := range map[string]string{
		"Cache-Control":       o.CacheControl,
		"Content-Disposition": o.ContentDisposition,
		"Content-Language":    o.ContentLanguage,
		"Content-Encoding":    o.ContentEncoding,
	} {
		if v != "" {
			h.Set(k, v)
		}
	}
	if o.CustomTime != "" {
		h.Set("X-Goog-Custom-Time", o.CustomTime)
	}
	for k, v := range o.Metadata {
		h.Set("X-Goog-Meta-"+k, v)
	}
}

// byteRange is a resolved request range [start, end].
type byteRange struct{ start, end int64 }

// parseRange resolves a single "bytes=" range against size. ok=false
// means the header is absent or unparsable (serve everything);
// satisfiable=false means 416.
func parseRange(h string, size int64) (br byteRange, ok, satisfiable bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return br, false, true
	}
	a, b, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		// "bytes=-N" written by some clients as "bytes=N" with negative sign.
		if n, err := strconv.ParseInt(spec, 10, 64); err == nil && n < 0 {
			a, b = "", strconv.FormatInt(-n, 10)
		} else {
			return br, false, true
		}
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil {
			return br, false, true
		}
		if n == 0 {
			return br, true, false
		}
		if n > size {
			n = size
		}
		return byteRange{size - n, size - 1}, true, size > 0
	default:
		start, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return br, false, true
		}
		end := size - 1
		if b != "" {
			if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
				return br, false, true
			}
			if end > size-1 {
				end = size - 1
			}
		}
		if start >= size {
			return br, true, false
		}
		return byteRange{start, end}, true, true
	}
}

// serveMedia streams an object's data honouring Range, HEAD and gzip
// decompressive transcoding.
func (s *Service) serveMedia(w http.ResponseWriter, r *http.Request, rec *objectRec) error {
	o := rec.Object
	f, err := s.blobs.open(rec.Blob)
	if err != nil {
		return apierr.Internal("object data unavailable: %v", err)
	}
	defer f.Close()
	h := w.Header()
	setObjectHeaders(h, o)
	size := int64(o.Size)

	// Decompressive transcoding: gzip objects are served decompressed to
	// clients that do not accept gzip; Range is ignored in that case.
	if o.ContentEncoding == "gzip" && !acceptsGzip(r) {
		h.Del("Content-Encoding")
		h.Del("Accept-Ranges")
		h.Set("Warning", "214 UploadServer gunzipped")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return nil
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil
		}
		defer zr.Close()
		copyOut(w, zr)
		return nil
	}

	status := http.StatusOK
	start, length := int64(0), size
	if br, ok, satisfiable := parseRange(r.Header.Get("Range"), size); ok {
		if !satisfiable {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			return apierr.New(codes.OutOfRange, "The requested range cannot be satisfied.").WithLegacy("requestedRangeNotSatisfiable").WithHTTP(http.StatusRequestedRangeNotSatisfiable)
		}
		status = http.StatusPartialContent
		start, length = br.start, br.end-br.start+1
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", br.start, br.end, size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead || length == 0 {
		return nil
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil
		}
	}
	copyOut(w, io.LimitReader(f, length))
	return nil
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(strings.SplitN(p, ";", 2)[0]), "gzip") {
				return true
			}
		}
	}
	return false
}

// copyOut streams src to w with a large buffer.
func copyOut(w io.Writer, src io.Reader) {
	bp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bp)
	_, _ = io.CopyBuffer(writerOnly{w}, src, *bp)
}
