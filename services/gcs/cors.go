package gcs

import (
	"net/http"
	"strconv"
	"strings"

	storage "google.golang.org/api/storage/v1"
)

// Bucket CORS (FR-GCS-001) applied to XML API requests from browsers.

// matchCORS returns the first CORS entry allowing origin and method.
func matchCORS(b *storage.Bucket, origin, method string) *storage.BucketCors {
	for _, c := range b.Cors {
		if (contains(c.Origin, "*") || contains(c.Origin, origin)) && (contains(c.Method, "*") || containsFold(c.Method, method)) {
			return c
		}
	}
	return nil
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// applyCORS adds CORS response headers for a simple request.
func (s *Service) applyCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	rec, err := s.getBucketRec(bucket)
	if err != nil {
		return
	}
	c := matchCORS(rec.Bucket, origin, r.Method)
	if c == nil {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	if len(c.ResponseHeader) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(c.ResponseHeader, ", "))
	}
}

// corsPreflight answers an OPTIONS preflight request.
func (s *Service) corsPreflight(w http.ResponseWriter, r *http.Request, bucket string) {
	origin := r.Header.Get("Origin")
	method := r.Header.Get("Access-Control-Request-Method")
	rec, err := s.getBucketRec(bucket)
	if err != nil || origin == "" || method == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	c := matchCORS(rec.Bucket, origin, method)
	if c == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", strings.Join(c.Method, ", "))
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		h.Set("Access-Control-Allow-Headers", req)
	}
	if c.MaxAgeSeconds > 0 {
		h.Set("Access-Control-Max-Age", strconv.FormatInt(c.MaxAgeSeconds, 10))
	}
	h.Add("Vary", "Origin")
	w.WriteHeader(http.StatusOK)
}
