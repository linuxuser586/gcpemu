package gcs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// ServeHTTP dispatches JSON API, upload, download, batch and XML API requests.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		apierr.Write(w, apierr.New(codes.Unavailable, "Cloud Storage is starting."))
		return
	}
	p := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(p, "/storage/v1/"):
		s.serveJSON(w, r, strings.TrimPrefix(p, "/storage/v1/"))
	case strings.HasPrefix(p, "/upload/storage/v1/"):
		s.serveUpload(w, r, strings.TrimPrefix(p, "/upload/storage/v1/"))
	case strings.HasPrefix(p, "/download/storage/v1/"):
		s.serveDownloadPath(w, r, strings.TrimPrefix(p, "/download/storage/v1/"))
	case p == "/batch/storage/v1" || strings.HasPrefix(p, "/batch/storage/v1/"):
		s.serveBatch(w, r)
	default:
		s.serveXML(w, r)
	}
}

// splitPath splits an escaped path into unescaped segments.
func splitPath(p string) ([]string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, true
	}
	raw := strings.Split(p, "/")
	out := make([]string, len(raw))
	for i, seg := range raw {
		u, err := url.PathUnescape(seg)
		if err != nil {
			return nil, false
		}
		out[i] = u
	}
	return out, true
}

// joinUnescaped joins escaped segments into one unescaped string (object
// names whose slashes were not escaped by the client).
func joinUnescaped(segs []string) (string, bool) {
	u, err := url.PathUnescape(strings.Join(segs, "/"))
	return u, err == nil
}

func (s *Service) serveJSON(w http.ResponseWriter, r *http.Request, rest string) {
	esc := strings.Split(strings.Trim(rest, "/"), "/")
	segs, ok := splitPath(rest)
	if !ok {
		apierr.Write(w, errInvalid("Invalid path."))
		return
	}
	m := r.Method
	n := len(segs)
	switch {
	case n == 1 && segs[0] == "b":
		switch m {
		case http.MethodGet:
			s.listBuckets(w, r)
			return
		case http.MethodPost:
			s.insertBucket(w, r)
			return
		}
	case n >= 2 && segs[0] == "b":
		s.serveBucketPath(w, r, segs[1], esc[2:], segs[2:])
		return
	case n >= 3 && segs[0] == "projects" && segs[2] == "hmacKeys":
		s.serveHMAC(w, r, segs[1], segs[3:])
		return
	case n == 3 && segs[0] == "projects" && segs[2] == "serviceAccount" && m == http.MethodGet:
		s.getServiceAccount(w, r, segs[1])
		return
	}
	methodNotAllowed(w, r)
}

func (s *Service) serveBucketPath(w http.ResponseWriter, r *http.Request, bucket string, esc, segs []string) {
	m := r.Method
	n := len(segs)
	switch {
	case n == 0:
		switch m {
		case http.MethodGet:
			s.getBucket(w, r, bucket)
		case http.MethodPatch:
			s.patchBucket(w, r, bucket, false)
		case http.MethodPut:
			s.patchBucket(w, r, bucket, true)
		case http.MethodDelete:
			s.deleteBucket(w, r, bucket)
		default:
			methodNotAllowed(w, r)
		}
	case segs[0] == "iam":
		switch {
		case n == 1 && m == http.MethodGet:
			s.getBucketIAM(w, r, bucket)
		case n == 1 && m == http.MethodPut:
			s.setBucketIAM(w, r, bucket)
		case n == 2 && segs[1] == "testPermissions" && m == http.MethodGet:
			s.testBucketIAM(w, r, bucket)
		default:
			methodNotAllowed(w, r)
		}
	case n == 1 && segs[0] == "lockRetentionPolicy" && m == http.MethodPost:
		s.lockRetentionPolicy(w, r, bucket)
	case segs[0] == "notificationConfigs":
		switch {
		case n == 1 && m == http.MethodGet:
			s.listNotifications(w, r, bucket)
		case n == 1 && m == http.MethodPost:
			s.insertNotification(w, r, bucket)
		case n == 2 && m == http.MethodGet:
			s.getNotification(w, r, bucket, segs[1])
		case n == 2 && m == http.MethodDelete:
			s.deleteNotification(w, r, bucket, segs[1])
		default:
			methodNotAllowed(w, r)
		}
	case segs[0] == "o":
		s.serveObjectPath(w, r, bucket, esc[1:])
	default:
		methodNotAllowed(w, r)
	}
}

// serveObjectPath routes ".../b/B/o[/OBJECT[/compose|/copyTo/b/B/o/O|/rewriteTo/...]]".
// esc holds the escaped segments after "o".
func (s *Service) serveObjectPath(w http.ResponseWriter, r *http.Request, bucket string, esc []string) {
	m := r.Method
	if len(esc) == 0 || (len(esc) == 1 && esc[0] == "") {
		switch m {
		case http.MethodGet:
			s.listObjects(w, r, bucket)
		case http.MethodPost:
			s.insertObjectMedia(w, r, bucket)
		default:
			methodNotAllowed(w, r)
		}
		return
	}
	if m == http.MethodPost {
		for i := 1; i+3 < len(esc); i++ {
			if (esc[i] == "copyTo" || esc[i] == "rewriteTo") && esc[i+1] == "b" && esc[i+3] == "o" && i+4 < len(esc) {
				src, ok1 := joinUnescaped(esc[:i])
				dstBucket, ok2 := url.PathUnescape(esc[i+2])
				dst, ok3 := joinUnescaped(esc[i+4:])
				if !ok1 || ok2 != nil || !ok3 {
					break
				}
				if esc[i] == "copyTo" {
					s.copyObject(w, r, bucket, src, dstBucket, dst)
				} else {
					s.rewriteObject(w, r, bucket, src, dstBucket, dst)
				}
				return
			}
		}
		if len(esc) >= 2 && esc[len(esc)-1] == "compose" {
			name, ok := joinUnescaped(esc[:len(esc)-1])
			if ok {
				s.composeObject(w, r, bucket, name)
				return
			}
		}
		methodNotAllowed(w, r)
		return
	}
	name, ok := joinUnescaped(esc)
	if !ok {
		apierr.Write(w, errInvalid("Invalid object name."))
		return
	}
	switch m {
	case http.MethodGet:
		s.getObject(w, r, bucket, name)
	case http.MethodPatch:
		s.patchObject(w, r, bucket, name, false)
	case http.MethodPut:
		s.patchObject(w, r, bucket, name, true)
	case http.MethodDelete:
		s.deleteObject(w, r, bucket, name)
	default:
		methodNotAllowed(w, r)
	}
}

// serveDownloadPath handles "/download/storage/v1/b/B/o/O".
func (s *Service) serveDownloadPath(w http.ResponseWriter, r *http.Request, rest string) {
	esc := strings.Split(strings.Trim(rest, "/"), "/")
	if len(esc) < 4 || esc[0] != "b" || esc[2] != "o" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		methodNotAllowed(w, r)
		return
	}
	bucket, err := url.PathUnescape(esc[1])
	name, ok := joinUnescaped(esc[3:])
	if err != nil || !ok {
		apierr.Write(w, errInvalid("Invalid path."))
		return
	}
	s.downloadJSON(w, r, bucket, name)
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	apierr.Write(w, apierr.NotFound("Not Found").WithLegacy("notFound"))
}

// writeJSON writes v with the JSON API content type.
func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		apierr.Write(w, apierr.Internal("encode response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// decodeJSON decodes an optional JSON request body into v.
func decodeJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return errInvalid("Failed to read request body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errInvalid("Invalid JSON payload received. %v", err)
	}
	return nil
}

// readJSONBody reads the raw request body (for merge patches).
func readJSONBody(r *http.Request) (map[string]json.RawMessage, []byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return nil, nil, errInvalid("Failed to read request body: %v", err)
	}
	m := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, nil, errInvalid("Invalid JSON payload received. %v", err)
		}
	}
	return m, b, nil
}
