package ar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Pull-through cache shared by the containerd mirror (mirror.go) and
// remote repositories (remote.go) (FR-AR-006, FR-GKE-006).
//
// Cached content lives in the ordinary registry tables under a cache
// scope: the remote repository itself (so cached images show up as its
// packages and are deleted with it) or, for mirror requests, a hidden
// per-upstream-host scope that is never listed. Blob bytes go to the
// shared content-addressed blob store, so a layer is stored once however
// many scopes reference it, and the cache survives restarts with
// --data-dir.
//
// Manifests fetched by digest are immutable. Tags are revalidated against
// the upstream (HEAD, which Docker Hub does not count against its pull
// rate limit) once their entry is older than the tag TTL; when the
// upstream is unreachable, rate limited or the emulator is offline the
// cached entry is served stale. Every manifest and blob is verified
// against its digest before it is committed. Blobs are streamed to the
// client while they are written to the cache, and concurrent requests
// for the same manifest or blob share one upstream fetch.

// mirrorProject is the project segment of mirror cache scopes. It is not
// a valid project ID, so it never collides with a repository.
const mirrorProject = "_mirror"

// fetchTimeout bounds an upstream manifest fetch.
const fetchTimeout = 2 * time.Minute

// defaultTagTTL is how long a cached tag is served before revalidation.
const defaultTagTTL = 5 * time.Minute

// manifestAccept lists every manifest type the cache stores.
var manifestAccept = strings.Join([]string{mtOCIIndex, mtDockerList, mtOCIManifest, mtDockerManifest}, ", ")

// pullThrough is one image whose cache lives in scope/img and whose
// content comes from up.
type pullThrough struct {
	scope repoRef
	img   string
	up    upstreamRef
}

// SetMirrorTagTTL sets how long cached tags are served before they are
// revalidated with the upstream (default 5m; 0 revalidates every pull).
func (s *Service) SetMirrorTagTTL(d time.Duration) {
	s.mu.Lock()
	s.tagTTL = d
	s.mu.Unlock()
}

// background returns the context upstream fetches run under; it is
// cancelled when the service stops.
func (s *Service) background() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bgCtx
}

func (s *Service) offline() bool { return s.env.Config.Offline }

func (s *Service) tagTTLValue() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tagTTL
}

// errNotCached is returned for content that is neither cached nor
// fetchable because the emulator is offline.
var errNotCached = errors.New("not cached and the emulator is offline")

// ptManifest returns the cached or fetched manifest for a tag or digest.
//
// Upstream fetches run detached from the request (bounded by
// fetchTimeout) so that a client going away does not fail the requests
// coalesced onto its fetch.
func (s *Service) ptManifest(pt *pullThrough, ref string) (*manifestRec, []byte, error) {
	if isDigestRef(ref) {
		if m, body, ok := s.cachedManifest(pt, ref); ok {
			return m, body, nil
		}
		if s.offline() {
			return nil, nil, errNotCached
		}
		v, err := s.flights.do("m|"+pt.scope.key()+"|"+pt.img+"|"+ref, func() (any, error) {
			ctx, cancel := context.WithTimeout(s.background(), fetchTimeout)
			defer cancel()
			return s.fetchManifest(ctx, pt, ref, "")
		})
		if err != nil {
			return nil, nil, err
		}
		r := v.(*fetchedManifest)
		return r.rec, r.body, nil
	}
	var tag *tagRec
	_ = s.env.Store.View(func(tx store.Tx) error { tag, _ = getTag(tx, pt.scope, pt.img, ref); return nil })
	var cm *manifestRec
	var cbody []byte
	if tag != nil {
		var ok bool
		if cm, cbody, ok = s.cachedManifest(pt, tag.Digest); !ok {
			cm, tag = nil, nil
		}
	}
	if cm != nil && (s.offline() || s.env.Clock.Now().Sub(tag.Updated) < s.tagTTLValue()) {
		return cm, cbody, nil
	}
	if s.offline() {
		return nil, nil, errNotCached
	}
	v, err := s.flights.do("t|"+pt.scope.key()+"|"+pt.img+"|"+ref, func() (any, error) {
		ctx, cancel := context.WithTimeout(s.background(), fetchTimeout)
		defer cancel()
		return s.revalidateTag(ctx, pt, ref, cm)
	})
	if err != nil {
		if cm != nil && !isNotFound(err) {
			s.env.Log.Warn("ar: serving stale cached tag", "image", pt.up.String()+":"+ref, "err", err)
			return cm, cbody, nil
		}
		return nil, nil, err
	}
	r := v.(*fetchedManifest)
	return r.rec, r.body, nil
}

type fetchedManifest struct {
	rec  *manifestRec
	body []byte
}

// revalidateTag checks a cached tag with HEAD and refetches it when the
// upstream digest changed (or nothing is cached).
func (s *Service) revalidateTag(ctx context.Context, pt *pullThrough, tag string, cached *manifestRec) (*fetchedManifest, error) {
	if cached != nil {
		resp, err := s.upstream.do(ctx, http.MethodHead, pt.up, "/v2/"+pt.up.Repo+"/manifests/"+tag, http.Header{"Accept": {manifestAccept}})
		if err != nil {
			return nil, err
		}
		drain(resp)
		if d := resp.Header.Get("Docker-Content-Digest"); d == cached.Digest {
			err := s.env.Store.Update(func(tx store.Tx) error {
				t, ok := getTag(tx, pt.scope, pt.img, tag)
				if !ok {
					return nil
				}
				t.Updated = s.env.Clock.Now()
				return store.PutJSON(tx, nsTags, itemKey(pt.scope, pt.img, tag), t)
			})
			if err != nil {
				return nil, err
			}
			body, err := s.blobs.read(cached.Digest)
			if err != nil {
				return nil, err
			}
			return &fetchedManifest{cached, body}, nil
		}
	}
	return s.fetchManifest(ctx, pt, tag, tag)
}

// cachedManifest returns a manifest cached in the scope.
func (s *Service) cachedManifest(pt *pullThrough, digest string) (*manifestRec, []byte, bool) {
	var m *manifestRec
	_ = s.env.Store.View(func(tx store.Tx) error { m, _ = getManifest(tx, pt.scope, pt.img, digest); return nil })
	if m == nil {
		return nil, nil, false
	}
	body, err := s.blobs.read(digest)
	if err != nil {
		return nil, nil, false
	}
	return m, body, true
}

// fetchManifest GETs a manifest from the upstream, verifies it and stores
// it (and the tag, if tag is not "").
func (s *Service) fetchManifest(ctx context.Context, pt *pullThrough, ref, tag string) (*fetchedManifest, error) {
	resp, err := s.upstream.do(ctx, http.MethodGet, pt.up, "/v2/"+pt.up.Repo+"/manifests/"+ref, http.Header{"Accept": {manifestAccept}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxManifest {
		return nil, fmt.Errorf("upstream manifest %s:%s exceeds %d bytes", pt.up, ref, maxManifest)
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if isDigestRef(ref) && ref != digest {
		return nil, fmt.Errorf("upstream manifest %s@%s has digest %s", pt.up, ref, digest)
	}
	if hd := resp.Header.Get("Docker-Content-Digest"); strings.HasPrefix(hd, "sha256:") && hd != digest {
		return nil, fmt.Errorf("upstream manifest %s:%s has digest %s, upstream declared %s", pt.up, ref, digest, hd)
	}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("upstream manifest %s:%s is invalid: %v", pt.up, ref, err)
	}
	mt := manifestType(resp.Header.Get("Content-Type"), &doc)
	if !isManifestType(mt) {
		return nil, fmt.Errorf("upstream manifest %s:%s has unsupported media type %q", pt.up, ref, mt)
	}
	now := s.env.Clock.Now()
	rec := &manifestRec{
		Digest: digest, MediaType: mt, Size: int64(len(body)), ArtifactType: doc.ArtifactType,
		Annotations: doc.Annotations, ContentSize: int64(len(body)), Created: now, Updated: now,
	}
	if doc.Subject != nil {
		rec.Subject = doc.Subject.Digest
	}
	for _, d := range doc.Manifests {
		c := childRec{Digest: d.Digest, MediaType: d.MediaType, Size: d.Size}
		if p := d.Platform; p != nil {
			c.OS, c.Architecture, c.Variant, c.OSVersion, c.OSFeatures = p.OS, p.Architecture, p.Variant, p.OSVersion, p.OSFeatures
		}
		rec.Children = append(rec.Children, c)
		rec.ContentSize += d.Size
	}
	if !isIndex(mt) && doc.Config != nil {
		for _, d := range append([]descriptor{*doc.Config}, doc.Layers...) {
			if !nonDistributable(d) {
				rec.Blobs = append(rec.Blobs, d.Digest)
				rec.ContentSize += d.Size
			}
		}
		if s.blobs.has(doc.Config.Digest) {
			rec.BuildTime = s.buildTime(doc.Config.Digest)
		}
	}
	s.blobs.gc.RLock()
	defer s.blobs.gc.RUnlock()
	if _, _, err := s.blobs.put(bytes.NewReader(body), digest); err != nil {
		return nil, err
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		if pt.scope.Project != mirrorProject {
			if _, err := getRepo(tx, pt.scope); err != nil {
				return err
			}
		}
		if old, ok := getManifest(tx, pt.scope, pt.img, digest); ok {
			rec.Created = old.Created
		}
		if tag != "" {
			tr := tagRec{Digest: digest, Created: now, Updated: now}
			if old, ok := getTag(tx, pt.scope, pt.img, tag); ok {
				tr.Created = old.Created
			}
			if err := store.PutJSON(tx, nsTags, itemKey(pt.scope, pt.img, tag), tr); err != nil {
				return err
			}
		}
		return putManifest(tx, pt.scope, pt.img, rec)
	})
	if err != nil {
		return nil, err
	}
	return &fetchedManifest{rec, body}, nil
}

// blobSource is a blob to serve: a complete file or an in-flight fetch.
type blobSource struct {
	file  *os.File
	fetch *blobFetch
}

// ptOpenBlob opens a blob from the cache or starts (or joins) its upstream
// fetch. The caller must call close on the result.
func (s *Service) ptOpenBlob(ctx context.Context, pt *pullThrough, digest string) (*blobSource, error) {
	if src, ok := s.cachedBlob(pt, digest); ok {
		return src, nil
	}
	if s.offline() {
		return nil, errNotCached
	}
	f := s.joinFetch(pt, digest)
	select {
	case <-f.ready:
	case <-ctx.Done():
		f.release()
		return nil, ctx.Err()
	}
	if err := f.result(); err != nil {
		f.release()
		// A concurrent request may already have cached the blob.
		if src, ok := s.cachedBlob(pt, digest); ok {
			return src, nil
		}
		return nil, err
	}
	return &blobSource{fetch: f}, nil
}

// cachedBlob opens a blob linked in the scope, linking a blob already in
// the store under another scope first.
func (s *Service) cachedBlob(pt *pullThrough, digest string) (*blobSource, bool) {
	s.blobs.gc.RLock()
	defer s.blobs.gc.RUnlock()
	var linked bool
	_ = s.env.Store.View(func(tx store.Tx) error { _, linked = getLink(tx, pt.scope, pt.img, digest); return nil })
	f, err := s.blobs.open(digest)
	if err != nil {
		return nil, false
	}
	if !linked {
		st, err := f.Stat()
		if err == nil {
			err = s.linkScope(pt.scope, pt.img, digest, st.Size())
		}
		if err != nil {
			f.Close()
			return nil, false
		}
	}
	return &blobSource{file: f}, true
}

// linkScope records a cached blob in a scope (caller holds gc for reading).
func (s *Service) linkScope(scope repoRef, img, digest string, size int64) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		if scope.Project != mirrorProject {
			if _, err := getRepo(tx, scope); err != nil {
				return err
			}
		}
		return putLink(tx, scope, img, digest, size, s.env.Clock.Now())
	})
}

func (b *blobSource) close() {
	if b.file != nil {
		b.file.Close()
	}
	if b.fetch != nil {
		b.fetch.release()
	}
}

// serve writes the blob response. In-flight blobs are streamed as they
// arrive; a Range request on one waits for it to complete.
func (s *Service) serveBlobSource(w http.ResponseWriter, r *http.Request, digest string, src *blobSource) {
	h := w.Header()
	h.Set("Docker-Content-Digest", digest)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Etag", `"`+digest+`"`)
	h.Set("Cache-Control", "max-age=31536000")
	if src.file != nil {
		http.ServeContent(w, r, "", time.Time{}, src.file)
		return
	}
	f := src.fetch
	if r.Header.Get("Range") != "" && r.Method == http.MethodGet {
		if !f.wait(r.Context()) {
			return
		}
		if err := f.result(); err != nil {
			writeRegError(w, regErr(http.StatusBadGateway, "UNKNOWN", "fetch blob %s: %v", digest, err))
			return
		}
		file, err := s.blobs.open(digest)
		if err != nil {
			writeRegError(w, regErr(http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry"))
			return
		}
		defer file.Close()
		http.ServeContent(w, r, "", time.Time{}, file)
		return
	}
	if f.size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(f.size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, 128<<10)
	var off int64
	for {
		f.mu.Lock()
		n, done, err, ch := f.written, f.done, f.err, f.notify
		f.mu.Unlock()
		if off < n {
			k := min(int64(len(buf)), n-off)
			m, rerr := f.file.ReadAt(buf[:k], off)
			if m > 0 {
				if _, werr := w.Write(buf[:m]); werr != nil {
					return
				}
				off += int64(m)
			}
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				panic(http.ErrAbortHandler)
			}
			if off >= n {
				_ = rc.Flush()
			}
			continue
		}
		if done {
			if err != nil {
				// Truncate the response so the client sees the failure.
				panic(http.ErrAbortHandler)
			}
			return
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
	}
}

// blobFetch is one upstream blob download shared by every request for the
// digest. Readers follow the temp file as it grows.
type blobFetch struct {
	s      *Service
	digest string
	ready  chan struct{} // closed once size is known or the fetch failed

	mu      sync.Mutex
	file    *os.File
	size    int64 // -1 if unknown
	written int64
	done    bool
	err     error
	notify  chan struct{} // closed and replaced on every progress update
	refs    int
	scopes  []scopeImg
}

type scopeImg struct {
	scope repoRef
	img   string
}

// joinFetch returns the in-flight fetch for digest, starting one if none
// is running, with a reference held for the caller.
func (s *Service) joinFetch(pt *pullThrough, digest string) *blobFetch {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fetches[digest]
	if f == nil {
		f = &blobFetch{s: s, digest: digest, ready: make(chan struct{}), size: -1, notify: make(chan struct{}), refs: 1}
		s.fetches[digest] = f
		go f.run(s.bgCtx, pt.up)
	}
	f.mu.Lock()
	f.refs++
	f.scopes = append(f.scopes, scopeImg{pt.scope, pt.img})
	f.mu.Unlock()
	return f
}

// wait blocks until the fetch completes; it reports false if ctx ended.
func (f *blobFetch) wait(ctx context.Context) bool {
	for {
		f.mu.Lock()
		done, ch := f.done, f.notify
		f.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

func (f *blobFetch) result() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// release drops a reference, closing the temp file after the last one.
func (f *blobFetch) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs--
	if f.refs == 0 && f.file != nil {
		_ = f.file.Close()
		f.file = nil
	}
}

// progress publishes new state to readers (caller holds f.mu).
func (f *blobFetch) progress() {
	close(f.notify)
	f.notify = make(chan struct{})
}

// run downloads, verifies and installs the blob, then links it into every
// scope that asked for it.
func (f *blobFetch) run(ctx context.Context, up upstreamRef) {
	s := f.s
	var tmp string
	readyOnce := sync.OnceFunc(func() { close(f.ready) })
	finish := func(err error) {
		f.mu.Lock()
		f.done, f.err = true, err
		f.progress()
		f.mu.Unlock()
		readyOnce()
		s.mu.Lock()
		if s.fetches[f.digest] == f {
			delete(s.fetches, f.digest)
		}
		s.mu.Unlock()
		if tmp != "" && err != nil {
			_ = os.Remove(tmp)
		}
		f.release()
	}
	resp, err := s.upstream.do(ctx, http.MethodGet, up, "/v2/"+up.Repo+"/blobs/"+f.digest, nil)
	if err != nil {
		finish(err)
		return
	}
	defer resp.Body.Close()
	file, err := os.CreateTemp(filepath.Join(s.blobs.dir, "tmp"), "fetch-")
	if err != nil {
		finish(err)
		return
	}
	tmp = file.Name()
	f.mu.Lock()
	f.file, f.size = file, resp.ContentLength
	f.mu.Unlock()
	readyOnce()

	h := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := file.Write(buf[:n]); werr != nil {
				finish(werr)
				return
			}
			h.Write(buf[:n])
			f.mu.Lock()
			f.written += int64(n)
			f.progress()
			f.mu.Unlock()
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			finish(fmt.Errorf("download blob %s from %s: %w", f.digest, up, rerr))
			return
		}
	}
	if d := "sha256:" + hex.EncodeToString(h.Sum(nil)); d != f.digest {
		finish(fmt.Errorf("upstream blob %s from %s has digest %s", f.digest, up, d))
		return
	}
	if f.size >= 0 && f.written != f.size {
		finish(fmt.Errorf("upstream blob %s from %s is truncated", f.digest, up))
		return
	}
	if err := file.Sync(); err != nil {
		finish(err)
		return
	}
	s.blobs.gc.RLock()
	err = s.blobs.install(tmp, f.digest)
	if err == nil {
		tmp = ""
		f.mu.Lock()
		scopes := append([]scopeImg(nil), f.scopes...)
		f.mu.Unlock()
		for _, si := range scopes {
			if lerr := s.linkScope(si.scope, si.img, f.digest, f.written); lerr != nil {
				s.env.Log.Debug("ar: link cached blob", "scope", si.scope.key(), "err", lerr)
			}
		}
	}
	s.blobs.gc.RUnlock()
	finish(err)
}

// flightGroup coalesces concurrent calls with the same key.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	v    any
	err  error
}

func (g *flightGroup) do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flightCall{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.v, c.err
	}
	c := &flightCall{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()
	c.v, c.err = fn()
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	close(c.done)
	return c.v, c.err
}

// writeManifest writes a manifest response after content negotiation.
func writeManifest(w http.ResponseWriter, r *http.Request, m *manifestRec, body []byte) {
	if !accepts(r, m.MediaType) {
		writeRegError(w, regErr(http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest of type %s found, but the Accept header does not support it", m.MediaType))
		return
	}
	h := w.Header()
	h.Set("Content-Type", m.MediaType)
	h.Set("Docker-Content-Digest", m.Digest)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Etag", `"`+m.Digest+`"`)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// ptError maps a pull-through failure to a registry error.
func ptError(err error, kind, ref string) *regError {
	code := "MANIFEST_UNKNOWN"
	if kind == "blob" {
		code = "BLOB_UNKNOWN"
	}
	var ue *upstreamError
	switch {
	case errors.Is(err, errNotCached):
		return regErr(http.StatusNotFound, code, "%s %s is not cached and the emulator is offline", kind, ref)
	case isNotFound(err):
		return regErr(http.StatusNotFound, code, "%s unknown: %s", kind, ref)
	case errors.As(err, &ue) && ue.status == http.StatusUnauthorized:
		return regErr(http.StatusForbidden, "DENIED", "upstream denied access to %s %s: %s", kind, ref, ue.msg)
	case errors.As(err, &ue) && ue.status == http.StatusTooManyRequests:
		return regErr(http.StatusTooManyRequests, "TOOMANYREQUESTS", "upstream rate limit exceeded for %s %s", kind, ref)
	case errors.Is(err, context.Canceled):
		return regErr(499, "UNKNOWN", "request canceled")
	}
	return regErr(http.StatusBadGateway, "UNAVAILABLE", "fetch %s %s from upstream: %v", kind, ref, err)
}

// ptServeManifest serves GET/HEAD of a pull-through manifest.
func (s *Service) ptServeManifest(w http.ResponseWriter, r *http.Request, pt *pullThrough, ref string) {
	if !isDigestRef(ref) && !tagRe.MatchString(ref) {
		writeRegError(w, regErr(http.StatusBadRequest, "TAG_INVALID", "invalid tag %q", ref))
		return
	}
	m, body, err := s.ptManifest(pt, ref)
	if err != nil {
		writeRegError(w, ptError(err, "manifest", ref))
		return
	}
	writeManifest(w, r, m, body)
}

// ptServeBlob serves GET/HEAD of a pull-through blob.
func (s *Service) ptServeBlob(w http.ResponseWriter, r *http.Request, pt *pullThrough, digest string) {
	if _, err := parseDigest(digest); err != nil {
		writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "%v", err))
		return
	}
	src, err := s.ptOpenBlob(r.Context(), pt, digest)
	if err != nil {
		writeRegError(w, ptError(err, "blob", digest))
		return
	}
	defer src.close()
	s.serveBlobSource(w, r, digest, src)
}
