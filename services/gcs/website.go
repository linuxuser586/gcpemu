package gcs

import (
	"net/http"
	"strings"
	"time"
)

// ServeObject serves an object of bucket to a load balancer backend bucket
// request (FR-INT-003): the request path maps to the object name; the
// bucket's website configuration applies (mainPageSuffix for "/" and
// directory paths, notFoundPage with status 404); Range, HEAD and
// conditional requests (If-None-Match, If-Modified-Since) are honoured.
// Errors use the XML API format, as storage.googleapis.com returns them to
// a load balancer.
func (s *Service) ServeObject(w http.ResponseWriter, r *http.Request, bucket, path string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	brec, err := s.getBucketRec(bucket)
	if err != nil {
		xmlError(w, err)
		return
	}
	var suffix, notFound string
	if web := brec.Bucket.Website; web != nil {
		suffix, notFound = web.MainPageSuffix, web.NotFoundPage
	}
	name := strings.TrimPrefix(path, "/")
	if name == "" || strings.HasSuffix(name, "/") {
		if suffix == "" {
			xmlError(w, errObjectNotFound(bucket, name))
			return
		}
		name += suffix
	}
	rec, _, err := s.readObject(bucket, name, 0, conds{})
	if err != nil && suffix != "" && !strings.HasSuffix(path, "/") {
		// A "directory" with an index page redirects to its slash form.
		if _, _, derr := s.readObject(bucket, name+"/"+suffix, 0, conds{}); derr == nil {
			loc := path + "/"
			if r.URL.RawQuery != "" {
				loc += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, loc, http.StatusMovedPermanently)
			return
		}
	}
	status := http.StatusOK
	if err != nil && notFound != "" {
		if nf, _, nerr := s.readObject(bucket, notFound, 0, conds{}); nerr == nil {
			rec, err, status = nf, nil, http.StatusNotFound
		}
	}
	if err != nil {
		xmlError(w, err)
		return
	}
	if status == http.StatusOK && notModified(r, rec.Object.Md5Hash, rec.Object.Updated) {
		h := w.Header()
		setObjectHeaders(h, rec.Object)
		h.Del("Content-Length")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if status != http.StatusOK {
		r = r.Clone(r.Context())
		r.Header.Del("Range")
		w = &statusOverride{ResponseWriter: w, status: status}
	}
	if err := s.serveMedia(w, r, rec); err != nil {
		xmlError(w, err)
	}
}

// notModified evaluates If-None-Match and If-Modified-Since.
func notModified(r *http.Request, md5, updated string) bool {
	etag := `"` + md5 + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, t := range strings.Split(inm, ",") {
			t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
			if t == "*" || t == etag {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		t, err := http.ParseTime(ims)
		if err == nil && !parseTS(updated).Truncate(time.Second).After(t) {
			return true
		}
	}
	return false
}

// statusOverride replaces the status of a 200 response (notFoundPage).
type statusOverride struct {
	http.ResponseWriter
	status int
}

func (o *statusOverride) WriteHeader(code int) {
	if code == http.StatusOK {
		code = o.status
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *statusOverride) Write(b []byte) (int, error) { return o.ResponseWriter.Write(b) }
