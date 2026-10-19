package gcs

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// ForeignBucket reports whether r reads from a bucket the Instance doesn't
// have, through the JSON API, its download path or the XML API. The Google
// front end's LAN listener reads those through from real GCS for callers
// without credentials, such as the Go module proxy's downloads (ADR 0021).
func (s *Service) ForeignBucket(r *http.Request) bool {
	bucket := readBucket(r.URL.EscapedPath())
	if bucket == "" {
		return false
	}
	found := false
	_ = s.env.Store.View(func(tx store.Tx) error {
		found = store.Exists(tx, nsBuckets, bucket)
		return nil
	})
	return !found
}

// readBucket returns the bucket a read on the escaped path p names, or ""
// for paths that name none or don't read (listings, uploads, batches).
func readBucket(p string) string {
	var esc string
	switch {
	case strings.HasPrefix(p, "/storage/v1/b/"):
		esc = strings.TrimPrefix(p, "/storage/v1/b/")
	case strings.HasPrefix(p, "/download/storage/v1/b/"):
		esc = strings.TrimPrefix(p, "/download/storage/v1/b/")
	case !xmlAPI(p):
		return ""
	default:
		esc = strings.TrimPrefix(p, "/")
	}
	seg, _, _ := strings.Cut(esc, "/")
	bucket, err := url.PathUnescape(seg)
	if err != nil {
		return ""
	}
	return bucket
}

// Signed reports whether r is an XML API request carrying a V4 signature,
// in its query or its Authorization header, which serveXML verifies (and
// refuses when malformed).
func (s *Service) Signed(r *http.Request) bool {
	if !xmlAPI(r.URL.EscapedPath()) {
		return false
	}
	q, err := signedURLParams(r.URL.Query())
	return q != nil || err != nil || headerSigParams(r) != nil
}

// xmlAPI reports whether the escaped path p is the XML API's rather than
// the JSON API's, its uploads, downloads or batches (ServeHTTP).
func xmlAPI(p string) bool {
	for _, api := range []string{"/storage/", "/upload/", "/download/", "/batch/"} {
		if strings.HasPrefix(p, api) {
			return false
		}
	}
	return true
}
