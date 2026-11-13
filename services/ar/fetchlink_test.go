package ar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// A caller that missed the Mirror cache while a fetch was running, but joins
// only once the fetch has linked the scopes it had, still gets its scope
// linked (#798). Before the fix the fetch stayed joinable after copying
// its scopes, so such a caller was added to a fetch that never linked
// it. The test holds that window open through s.hooks (ADR 0079), so it
// fails every time the fix is reverted, not on a lucky interleaving. Its
// upstream is served in memory, with no socket to reset (#972).
//
// #798's other window, a fetch ending before its first caller's scope
// was added, is closed twice over: joinFetch adds the scope before the
// fetch starts, and the fetch takes s.mu, which joinFetch holds, before
// copying its scopes. Undoing either alone loses no link in that window,
// so it has no test of its own.
func TestFetchLinksScopeOfCallerJoiningLate(t *testing.T) {
	s := startedService(t)
	var err error
	if s.blobs, err = newBlobStore(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	blob := []byte("blob")
	sum := sha256.Sum256(blob)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	const img = "library/app"
	scopeA := repoRef{mirrorProject, "docker.io", "a"}
	scopeB := repoRef{mirrorProject, "docker.io", "b"}

	requested := make(chan struct{}) // A's fetch asked the upstream
	release := make(chan struct{})   // lets the upstream answer
	missedB := make(chan struct{})   // B missed the Mirror cache
	linked := make(chan struct{})    // A's fetch linked the scopes it had
	bReturned := make(chan struct{}) // B's open returned
	var requestedOnce, linkedOnce sync.Once
	s.upstream.hc.Transport = handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		requestedOnce.Do(func() { close(requested) })
		<-release
		_, _ = w.Write(blob)
	})
	s.hooks.missedCache = func(scope repoRef) {
		if scope != scopeB {
			return
		}
		close(missedB)
		<-linked
	}
	s.hooks.linked = func() {
		// Only A's fetch, the first to link, waits: it stays where B could
		// find it until B has joined whichever fetch it joins and returned.
		linkedOnce.Do(func() {
			close(linked)
			<-bReturned
		})
	}

	open := func(scope repoRef) {
		up := upstreamRef{Host: "docker.io", Base: "http://upstream.invalid", Repo: img}
		src, err := s.ptOpenBlob(context.Background(), &pullThrough{scope: scope, img: img, up: up}, digest)
		if err != nil {
			t.Errorf("open %s in %s: %v", digest, scope.key(), err)
			return
		}
		src.close()
	}
	var wg sync.WaitGroup
	wg.Go(func() { open(scopeA) })
	wg.Go(func() {
		defer close(bReturned)
		<-requested // so B misses the cache
		open(scopeB)
	})
	<-missedB
	close(release)
	wg.Wait()
	if err := s.fetchRuns.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, scope := range []repoRef{scopeA, scopeB} {
			if _, ok := getLink(tx, scope, img, digest); !ok {
				t.Errorf("%s not linked in %s after every fetch ended", digest, scope.key())
			}
		}
		return nil
	})
}

// handlerTransport serves each request with its handler in memory, with
// no listener and no dial.
type handlerTransport http.HandlerFunc

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec.Result(), nil
}
