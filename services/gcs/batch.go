package gcs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Batch requests (/batch/storage/v1): a multipart/mixed body of embedded
// HTTP requests, each dispatched to the JSON API and answered in a
// multipart/mixed response.

const maxBatch = 100

// recorder captures a sub-request response.
type recorder struct {
	code int
	hdr  http.Header
	body bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.hdr }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (s *Service) serveBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r)
		return
	}
	mt, params, err := parseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		apierr.Write(w, errInvalid("Batch requests must be multipart/mixed."))
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	for n := 0; ; n++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			apierr.Write(w, errInvalid("Malformed batch body: %v", err))
			return
		}
		if n >= maxBatch {
			apierr.Write(w, errInvalid("Too many requests in batch (maximum %d).", maxBatch))
			return
		}
		cid := part.Header.Get("Content-ID")
		sub, err := http.ReadRequest(bufio.NewReader(part))
		rec := &recorder{hdr: http.Header{}}
		if err != nil {
			apierr.Write(rec, errInvalid("Malformed embedded request: %v", err))
		} else {
			u, perr := url.Parse(sub.RequestURI)
			if perr != nil {
				apierr.Write(rec, errInvalid("Invalid request URI."))
			} else {
				sub.URL = &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
				sub.RequestURI = sub.URL.RequestURI()
				sub.Host = r.Host
				sub = sub.WithContext(r.Context())
				s.ServeHTTP(rec, sub)
			}
		}
		if rec.code == 0 {
			rec.code = http.StatusOK
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", "application/http")
		if cid != "" {
			h.Set("Content-ID", "response-"+strings.Trim(cid, "<>"))
		}
		pw, _ := mw.CreatePart(h)
		fmt.Fprintf(pw, "HTTP/1.1 %d %s\r\n", rec.code, http.StatusText(rec.code))
		rec.hdr.Set("Content-Length", fmt.Sprint(rec.body.Len()))
		_ = rec.hdr.Write(pw)
		fmt.Fprint(pw, "\r\n")
		_, _ = pw.Write(rec.body.Bytes())
	}
	_ = mw.Close()
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Bytes())
}
