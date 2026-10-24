package gateway

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Buckets across links (ADR 0027): a Cloud Storage bucket name is global,
// so a request on a bucket names no Project. The API gateway routes it by
// the bucket's Project: that of a bucket this Instance has, or else the
// link whose Linked Instance has it, among the buckets of the Projects it
// shares, which it keeps this Instance up to date with.

// storageService is the Service that serves Cloud Storage.
const storageService = "gcs"

// requestBucket returns the bucket a Cloud Storage request names in its
// path: through the JSON API, its uploads and downloads, or the XML API,
// and so signed URLs too. A batch, the bucket collection, and a request
// to any other Service name none.
func (g *Gateway) requestBucket(r *http.Request) string {
	if isGRPC(r) || internalPath(r.URL.Path) {
		return ""
	}
	p := r.URL.EscapedPath()
	g.mu.RLock()
	_, hosted := g.hosts[hostOnly(r.Host)]
	owner := g.apiOwners[hostOnly(r.Host)]
	g.mu.RUnlock()
	xmlAPI := false
	switch {
	case hosted:
		if owner != storageService {
			return ""
		}
		xmlAPI = true
	default:
		switch _, pattern := g.mux.Handler(r); pattern {
		case "/":
			g.mu.RLock()
			xmlAPI = g.fallback == storageService
			g.mu.RUnlock()
		case "/storage/":
			p, xmlAPI = strings.TrimPrefix(p, "/storage"), true
		}
	}
	for _, api := range []string{"/storage/v1/b/", "/upload/storage/v1/b/", "/download/storage/v1/b/"} {
		if rest, ok := strings.CutPrefix(p, api); ok {
			return firstSegment(rest)
		}
	}
	if !xmlAPI || !isXMLPath(p) {
		return ""
	}
	return firstSegment(strings.TrimPrefix(p, "/"))
}

// isXMLPath reports whether the escaped path p is the XML API's rather
// than the JSON API's, its uploads, downloads or batches.
func isXMLPath(p string) bool {
	for _, api := range []string{"/storage/", "/upload/", "/download/", "/batch/"} {
		if strings.HasPrefix(p, api) {
			return false
		}
	}
	return true
}

// firstSegment returns the first segment of the escaped path p, unescaped.
func firstSegment(p string) string {
	seg, _, _ := strings.Cut(p, "/")
	s, err := url.PathUnescape(seg)
	if err != nil {
		return ""
	}
	return s
}

// localBucket returns the Project of the bucket, when this Instance has it.
func (g *Gateway) localBucket(bucket string) (string, bool) {
	svc, ok := g.env.Lookup(storageService)
	if !ok {
		return "", false
	}
	b, ok := svc.(emu.Buckets)
	if !ok {
		return "", false
	}
	return b.BucketProject(bucket)
}

// projectsOf returns the Projects r names (requestProjects), or else the
// Project of the bucket this Instance has that it names.
func (g *Gateway) projectsOf(r *http.Request) []string {
	ids := requestProjects(r)
	if len(ids) > 0 {
		return ids
	}
	if b := g.requestBucket(r); b != "" {
		if id, ok := g.localBucket(b); ok {
			return addProject(nil, id)
		}
	}
	return nil
}

// bucketLink returns the link to which a request on a bucket this
// Instance does not have is forwarded: the link whose Linked Instance has
// it, in a Project delegated to that link.
func (g *Gateway) bucketLink(r *http.Request, l Links) string {
	b := g.requestBucket(r)
	if b == "" {
		return ""
	}
	if name, id, ok := l.BucketOwner(b); ok {
		if to, _ := l.DelegatedTo(id); to == name {
			return name
		}
	}
	return ""
}

// createdBucket returns the bucket that r creates, and whether it does so
// through the XML API rather than the JSON API.
func (g *Gateway) createdBucket(r *http.Request) (name string, xmlAPI bool) {
	switch {
	case r.Method == http.MethodPost && bucketCollection.MatchString(r.URL.Path):
		var b struct{ Name string }
		_ = json.Unmarshal(peekBody(r), &b)
		return b.Name, false
	case r.Method == http.MethodPut && r.URL.RawQuery == "" && r.Header.Get("X-Goog-Project-Id") != "" && xmlBucket(r.URL.Path):
		return g.requestBucket(r), true
	}
	return "", false
}

// bucketChange reports whether r creates or deletes a bucket, after which
// the front Instance asks the link it forwarded r over for its buckets.
func (g *Gateway) bucketChange(r *http.Request) bool {
	if b, _ := g.createdBucket(r); b != "" {
		return true
	}
	if r.Method != http.MethodDelete || g.requestBucket(r) == "" {
		return false
	}
	p := strings.TrimSuffix(r.URL.EscapedPath(), "/")
	for _, prefix := range []string{"/storage/storage/v1/b/", "/storage/v1/b/", "/upload/", "/download/", "/storage/", "/"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			return !strings.HasSuffix(prefix, "load/") && !strings.Contains(rest, "/")
		}
	}
	return false
}

// bucketTaken answers r, which creates a bucket, with 409 when its name is
// taken on the other side of a link, as GCP answers for a name taken
// globally: on a Linked Instance for a local bucket, or on this Instance
// for a bucket in a Delegated Project.
func (g *Gateway) bucketTaken(w http.ResponseWriter, r *http.Request, l Links, to string) bool {
	b, xmlAPI := g.createdBucket(r)
	if b == "" {
		return false
	}
	taken := false
	if to == "" {
		_, _, taken = l.BucketOwner(b)
	} else {
		_, taken = g.localBucket(b)
	}
	if !taken {
		return false
	}
	const msg = "The requested bucket name is not available. The bucket namespace is shared by all users of the system. Please select a different name and try again."
	if xmlAPI {
		w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
		w.WriteHeader(http.StatusConflict)
		_ = xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"Error"`
			Code    string   `xml:"Code"`
			Message string   `xml:"Message"`
		}{Code: "BucketAlreadyExists", Message: msg})
		return true
	}
	apierr.Write(w, apierr.AlreadyExists(msg))
	return true
}
