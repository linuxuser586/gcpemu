package ar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Every scope that pulls a blob through an upstream fetch has it linked
// once the fetch ends, however the fetch and its callers interleave
// (#798). The first caller's scope was registered only after the fetch
// started, and a caller joining once the fetch had linked its scopes was
// never linked; with tiny blobs from a local upstream, either loses links.
func TestFetchLinksEveryScope(t *testing.T) {
	s := startedService(t)
	var err error
	if s.blobs, err = newBlobStore(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	blobs := map[string][]byte{}
	for i := range 200 {
		b := fmt.Appendf(nil, "blob %d", i)
		sum := sha256.Sum256(b)
		blobs["sha256:"+hex.EncodeToString(sum[:])] = b
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := blobs[path.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(up.Close)
	const img = "library/app"
	scopes := []repoRef{{mirrorProject, "docker.io", "a"}, {mirrorProject, "docker.io", "b"}}
	var wg sync.WaitGroup
	for d := range blobs {
		for _, scope := range scopes {
			wg.Go(func() {
				pt := &pullThrough{scope: scope, img: img, up: upstreamRef{Host: "docker.io", Base: up.URL, Repo: img}}
				src, err := s.ptOpenBlob(context.Background(), pt, d)
				if err != nil {
					t.Errorf("open %s in %s: %v", d, scope.key(), err)
					return
				}
				src.close()
			})
		}
	}
	wg.Wait()
	if err := s.fetchRuns.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	missing := 0
	_ = s.env.Store.View(func(tx store.Tx) error {
		for d := range blobs {
			for _, scope := range scopes {
				if _, ok := getLink(tx, scope, img, d); !ok {
					missing++
				}
			}
		}
		return nil
	})
	if missing > 0 {
		t.Fatalf("%d of %d links missing after every fetch ended", missing, len(blobs)*len(scopes))
	}
}
