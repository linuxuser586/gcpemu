package ar_test

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/services/ar"
)

// fakeUpstream is a public registry (go-containerregistry's reference
// registry) that requires the Docker anonymous token flow, plus an
// unauthenticated side door used by tests to push content.
type fakeUpstream struct {
	t     *testing.T
	pub   *httptest.Server // token-protected, what the emulator pulls from
	push  *httptest.Server // same storage, no auth
	down  atomic.Bool      // answer 503 to everything
	delay atomic.Int64     // nanoseconds to stall blob GETs

	tokens, manifestGETs, manifestHEADs, blobGETs atomic.Int64
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	u := &fakeUpstream{t: t}
	u.push = httptest.NewServer(reg)
	u.pub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/token" {
			u.tokens.Add(1)
			if !strings.HasPrefix(r.URL.Query().Get("scope"), "repository:") || r.URL.Query().Get("service") != "fake-registry" {
				http.Error(w, "bad token request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"secret-token","expires_in":300}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			repo := strings.TrimPrefix(r.URL.Path, "/v2/")
			if i := strings.Index(repo, "/manifests/"); i >= 0 {
				repo = repo[:i]
			} else if i := strings.Index(repo, "/blobs/"); i >= 0 {
				repo = repo[:i]
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="fake-registry",scope="repository:`+repo+`:pull"`)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/manifests/") && r.Method == http.MethodGet:
			u.manifestGETs.Add(1)
		case strings.Contains(r.URL.Path, "/manifests/") && r.Method == http.MethodHead:
			u.manifestHEADs.Add(1)
		case strings.Contains(r.URL.Path, "/blobs/") && r.Method == http.MethodGet:
			u.blobGETs.Add(1)
			if d := u.delay.Load(); d > 0 {
				time.Sleep(time.Duration(d))
			}
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(u.pub.Close)
	t.Cleanup(u.push.Close)
	return u
}

// host is the token-protected upstream host:port.
func (u *fakeUpstream) host() string { return strings.TrimPrefix(u.pub.URL, "http://") }

// put pushes an image to repo:tag on the upstream and returns it.
func (u *fakeUpstream) put(repoTag string, size int64, layers int64) v1.Image {
	u.t.Helper()
	img, err := random.Image(size, layers)
	if err != nil {
		u.t.Fatal(err)
	}
	ref, err := name.ParseReference(strings.TrimPrefix(u.push.URL, "http://")+"/"+repoTag, name.Insecure)
	if err != nil {
		u.t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		u.t.Fatalf("push to upstream: %v", err)
	}
	return img
}

// putIndex pushes a multi-platform index to repo:tag on the upstream.
func (u *fakeUpstream) putIndex(repoTag string) v1.ImageIndex {
	u.t.Helper()
	idx, err := random.Index(256, 1, 2)
	if err != nil {
		u.t.Fatal(err)
	}
	ref, err := name.ParseReference(strings.TrimPrefix(u.push.URL, "http://")+"/"+repoTag, name.Insecure)
	if err != nil {
		u.t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		u.t.Fatalf("push index to upstream: %v", err)
	}
	return idx
}

// arService returns the running ar service of an instance.
func arService(t *testing.T, inst *emutest.Instance) *ar.Service {
	t.Helper()
	svc, ok := inst.Env.Lookup("ar")
	if !ok {
		t.Fatal("ar service not running")
	}
	return svc.(*ar.Service)
}

// mirrorTransport makes go-containerregistry behave like containerd with
// the emulator configured as a mirror: every registry host is sent to the
// emulator, with the original registry in the ns parameter.
type mirrorTransport struct {
	registry string
	ns       string
}

func (m *mirrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if r.URL.Path != "/v2/token" {
		q := r.URL.Query()
		q.Set("ns", m.ns)
		r.URL.RawQuery = q.Encode()
	}
	r.URL.Scheme, r.URL.Host, r.Host = "http", m.registry, m.registry
	return http.DefaultTransport.RoundTrip(r)
}
