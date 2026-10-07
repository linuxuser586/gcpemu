package ar_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

const allManifests = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// containerdGet issues an anonymous request the way containerd does for a
// mirror endpoint.
func containerdGet(t *testing.T, method, url string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", allManifests+", */*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp, b
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// pullAll fetches the manifest and every blob of img through the
// containerd-style mirror path and verifies digests.
func pullAll(t *testing.T, base, repo, ref, ns string, img v1.Image) {
	t.Helper()
	q := "?ns=" + ns
	resp, body := containerdGet(t, http.MethodHead, base+"/v2/"+repo+"/manifests/"+ref+q)
	want, _ := img.Digest()
	if resp.StatusCode != 200 || resp.Header.Get("Docker-Content-Digest") != want.String() {
		t.Fatalf("HEAD manifest = %d %s %s", resp.StatusCode, resp.Header.Get("Docker-Content-Digest"), body)
	}
	resp, body = containerdGet(t, http.MethodGet, base+"/v2/"+repo+"/manifests/"+ref+q)
	if resp.StatusCode != 200 || sha256Hex(body) != want.String() {
		t.Fatalf("GET manifest = %d %s", resp.StatusCode, body)
	}
	cfg, _ := img.ConfigName()
	digests := []string{cfg.String()}
	layers, _ := img.Layers()
	for _, l := range layers {
		d, _ := l.Digest()
		digests = append(digests, d.String())
	}
	for _, d := range digests {
		resp, body := containerdGet(t, http.MethodGet, base+"/v2/"+repo+"/blobs/"+d+q)
		if resp.StatusCode != 200 || sha256Hex(body) != d {
			t.Fatalf("GET blob %s = %d (%d bytes, digest %s)", d, resp.StatusCode, len(body), sha256Hex(body))
		}
	}
}

// FR-GKE-006: containerd mirror requests (ns parameter) are pulled through
// the cache with the upstream's anonymous token flow, then served from the
// cache, also while the upstream is down.
func TestMirrorContainerdPullThrough(t *testing.T) {
	up := newFakeUpstream(t)
	img := up.put("library/app:v1", 2048, 2)
	e := start(t, emutest.WithIAMMode(config.IAMEnforce))
	base := "http://" + e.registry()

	pullAll(t, base, "library/app", "v1", up.host(), img)
	if up.tokens.Load() == 0 {
		t.Fatal("upstream token endpoint was never used")
	}
	gets, blobs := up.manifestGETs.Load(), up.blobGETs.Load()
	if gets != 1 || blobs != 3 {
		t.Fatalf("upstream manifest GETs = %d, blob GETs = %d; want 1 and 3", gets, blobs)
	}
	// Second pull: cache hits within the tag TTL.
	pullAll(t, base, "library/app", "v1", up.host(), img)
	if up.manifestGETs.Load() != gets || up.blobGETs.Load() != blobs || up.manifestHEADs.Load() != 0 {
		t.Fatalf("second pull reached upstream: GETs %d HEADs %d blobs %d", up.manifestGETs.Load(), up.manifestHEADs.Load(), up.blobGETs.Load())
	}
	// Upstream down and tag expired: served stale.
	arService(t, e.inst).SetMirrorTagTTL(0)
	up.down.Store(true)
	pullAll(t, base, "library/app", "v1", up.host(), img)
	d, _ := img.Digest()
	pullAll(t, base, "library/app", d.String(), up.host(), img)
	// Not cached and upstream down: a gateway error, not a 404.
	resp, body := containerdGet(t, http.MethodGet, base+"/v2/library/other/manifests/v1?ns="+up.host())
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "UNAVAILABLE") {
		t.Fatalf("uncached with upstream down = %d %s", resp.StatusCode, body)
	}
	up.down.Store(false)
	// Unknown upstream content is MANIFEST_UNKNOWN.
	resp, body = containerdGet(t, http.MethodGet, base+"/v2/library/missing/manifests/v1?ns="+up.host())
	if resp.StatusCode != 404 || !strings.Contains(string(body), "MANIFEST_UNKNOWN") {
		t.Fatalf("missing upstream = %d %s", resp.StatusCode, body)
	}
	// Mirrors are read-only.
	resp, _ = containerdGet(t, http.MethodPost, base+"/v2/library/app/blobs/uploads/?ns="+up.host())
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("push to mirror = %d", resp.StatusCode)
	}
}

// A tag is revalidated with HEAD once its TTL expired and refetched when
// the upstream moved it.
func TestMirrorTagRevalidation(t *testing.T) {
	up := newFakeUpstream(t)
	img1 := up.put("team/app:latest", 256, 1)
	e := start(t)
	arService(t, e.inst).SetMirrorTagTTL(0)
	base := "http://" + e.registry()
	pullAll(t, base, "team/app", "latest", up.host(), img1)
	pullAll(t, base, "team/app", "latest", up.host(), img1)
	if up.manifestHEADs.Load() == 0 || up.manifestGETs.Load() != 1 {
		t.Fatalf("unchanged tag: HEADs %d GETs %d", up.manifestHEADs.Load(), up.manifestGETs.Load())
	}
	img2 := up.put("team/app:latest", 256, 1)
	pullAll(t, base, "team/app", "latest", up.host(), img2)
	if up.manifestGETs.Load() != 2 {
		t.Fatalf("moved tag: GETs %d", up.manifestGETs.Load())
	}
}

// Concurrent requests for the same blob share one upstream download and
// are streamed while it is written to the cache.
func TestMirrorCoalescesConcurrentPulls(t *testing.T) {
	up := newFakeUpstream(t)
	img := up.put("library/big:v1", 4<<20, 1)
	e := start(t)
	base := "http://" + e.registry()
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	up.delay.Store(int64(300 * time.Millisecond))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(base + "/v2/library/big/blobs/" + ld.String() + "?ns=" + up.host())
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 || sha256Hex(b) != ld.String() {
				errs <- fmt.Errorf("status %d, %d bytes, err %v", resp.StatusCode, len(b), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := up.blobGETs.Load(); n != 1 {
		t.Fatalf("upstream blob GETs = %d, want 1", n)
	}
	// Range requests are served from the cached file.
	req, _ := http.NewRequest(http.MethodGet, base+"/v2/library/big/blobs/"+ld.String()+"?ns="+up.host(), nil)
	req.Header.Set("Range", "bytes=0-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(b) != 10 {
		t.Fatalf("range = %d, %d bytes", resp.StatusCode, len(b))
	}
}

// go-containerregistry through the emulator as a containerd-style mirror
// for docker.io (library/ prefix, index host aliases) and with the path
// form REGISTRY/docker.io/IMAGE, including a multi-platform index.
func TestMirrorGoContainerRegistry(t *testing.T) {
	up := newFakeUpstream(t)
	img := up.put("library/busybox:1.0", 512, 2)
	idx := up.putIndex("library/multi:1.0")
	e := start(t)
	arService(t, e.inst).SetUpstream("docker.io", up.pub.URL)

	rt := remote.WithTransport(&mirrorTransport{registry: e.registry(), ns: "docker.io"})
	got, err := remote.Image(name.MustParseReference("busybox:1.0"), rt)
	if err != nil {
		t.Fatalf("pull via mirror: %v", err)
	}
	assertSameImage(t, img, got)

	got, err = remote.Image(e.ref("docker.io/busybox:1.0"))
	if err != nil {
		t.Fatalf("pull via path form: %v", err)
	}
	assertSameImage(t, img, got)

	gi, err := remote.Index(e.ref("docker.io/library/multi:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := idx.Digest()
	if gd, _ := gi.Digest(); gd != wd {
		t.Fatalf("index digest = %s, want %s", gd, wd)
	}
	im, _ := gi.IndexManifest()
	child, err := gi.Image(im.Manifests[1].Digest)
	if err != nil {
		t.Fatal(err)
	}
	wantChild, _ := idx.Image(im.Manifests[1].Digest)
	assertSameImage(t, wantChild, child)
}

// assertSameImage compares digests and reads every layer (which verifies
// the bytes against their digests).
func assertSameImage(t *testing.T, want, got v1.Image) {
	t.Helper()
	wd, _ := want.Digest()
	gd, err := got.Digest()
	if err != nil || gd != wd {
		t.Fatalf("digest = %s (%v), want %s", gd, err, wd)
	}
	layers, err := got.Layers()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("read layer: %v", err)
		}
		rc.Close()
	}
	if _, err := got.RawConfigFile(); err != nil {
		t.Fatal(err)
	}
}

// FR-GKE-006: the cache persists in the data dir and serves pulls when the
// emulator is offline and the upstream is gone.
func TestMirrorOfflinePersistentCache(t *testing.T) {
	up := newFakeUpstream(t)
	img := up.put("library/pause:3.10", 256, 1)
	dir := t.TempDir()
	persistent := func(offline bool) emutest.Option {
		return func(c *config.Config) { c.Ephemeral, c.DataDir, c.Offline = false, dir, offline }
	}
	e1 := start(t, persistent(false))
	ref := "127.0.0.1:" + strings.Split(up.host(), ":")[1]
	pullAll(t, "http://"+e1.registry(), "library/pause", "3.10", ref, img)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e1.inst.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	up.pub.Close()

	e2 := start(t, persistent(true))
	base := "http://" + e2.registry()
	pullAll(t, base, "library/pause", "3.10", ref, img)
	resp, body := containerdGet(t, http.MethodGet, base+"/v2/library/pause/manifests/3.9?ns="+ref)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "offline") {
		t.Fatalf("uncached offline = %d %s", resp.StatusCode, body)
	}
}
