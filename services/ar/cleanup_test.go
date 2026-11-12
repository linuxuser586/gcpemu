package ar_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/linuxuser586/gcpemu/emutest"
)

// #1045: deleting a version frees its layers and config at once.

// blobFile is where the registry keeps the content of digest on disk.
func (e *env) blobFile(d v1.Hash) string {
	return filepath.Join(e.inst.Internal().Env.DataDir, "ar", "blobs", "sha256", d.Hex[:2], d.Hex)
}

func (e *env) hasBlob(d v1.Hash) bool {
	_, err := os.Stat(e.blobFile(d))
	return err == nil
}

// randomLayer returns a fresh random layer.
func randomLayer(t *testing.T) v1.Layer {
	t.Helper()
	l, err := random.Layer(512, types.OCILayer)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// imageOf builds an image of layers, with a config unique to name.
func imageOf(t *testing.T, name string, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Labels: map[string]string{"name": name}})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// push writes img to PROJECT/images/<repoImage> (":tag" or "@digest").
func (e *env) push(repoImage string, img v1.Image) {
	e.t.Helper()
	if err := remote.Write(e.ref(testProject+"/images/"+repoImage), img); err != nil {
		e.t.Fatalf("push %s: %v", repoImage, err)
	}
}

// blobsOf returns the config and layer digests of img.
func blobsOf(t *testing.T, img v1.Image) (cfg v1.Hash, layers []v1.Hash) {
	t.Helper()
	cfg, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	ls, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		d, err := l.Digest()
		if err != nil {
			t.Fatal(err)
		}
		layers = append(layers, d)
	}
	return cfg, layers
}

func digestOf(t *testing.T, img interface{ Digest() (v1.Hash, error) }) v1.Hash {
	t.Helper()
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// deleteVersion deletes PROJECT/images/<img>@digest through the API, with force.
func (e *env) deleteVersion(img string, d v1.Hash) {
	e.t.Helper()
	name := "projects/" + testProject + "/locations/" + testLoc + "/repositories/images/packages/" +
		strings.ReplaceAll(img, "/", "%2F") + "/versions/" + d.String()
	op, err := e.c.DeleteVersion(e.ctx, &artifactregistrypb.DeleteVersionRequest{Name: name, Force: true})
	if err != nil {
		e.t.Fatalf("DeleteVersion %s: %v", name, err)
	}
	if err := op.Wait(e.ctx); err != nil {
		e.t.Fatalf("DeleteVersion %s wait: %v", name, err)
	}
}

// pullAll pulls an image and reads every layer.
func (e *env) pullAll(ref string) {
	e.t.Helper()
	img, err := remote.Image(e.ref(testProject + "/images/" + ref))
	if err != nil {
		e.t.Fatalf("pull %s: %v", ref, err)
	}
	ls, err := img.Layers()
	if err != nil {
		e.t.Fatalf("pull %s layers: %v", ref, err)
	}
	for _, l := range ls {
		rc, err := l.Compressed()
		if err != nil {
			e.t.Fatalf("pull %s layer: %v", ref, err)
		}
		_ = rc.Close()
	}
}

// Image A (config + 2 layers) and B under the same image path share one
// layer; deleting A's version frees A's config and unshared layer at once,
// keeps the shared one, and B still pulls.
func TestDeleteVersionFreesUnsharedBlobs(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	shared, own := randomLayer(t), randomLayer(t)
	a := imageOf(t, "a", shared, own)
	b := imageOf(t, "b", shared, randomLayer(t))
	e.push("app:a", a)
	e.push("app:b", b)
	aCfg, aLayers := blobsOf(t, a)

	e.deleteVersion("app", digestOf(t, a))

	if e.hasBlob(aCfg) {
		t.Error("A's config blob is still on disk")
	}
	if e.hasBlob(aLayers[1]) {
		t.Error("A's unshared layer is still on disk")
	}
	if !e.hasBlob(aLayers[0]) {
		t.Error("the layer shared with B was removed")
	}
	if e.hasBlob(digestOf(t, a)) {
		t.Error("A's manifest is still on disk")
	}
	e.pullAll("app:b")
}

// The same through a registry manifest DELETE by digest.
func TestRegistryManifestDeleteFreesUnsharedBlobs(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	shared, own := randomLayer(t), randomLayer(t)
	a := imageOf(t, "a", shared, own)
	e.push("app:a", a)
	e.push("app:b", imageOf(t, "b", shared))
	aCfg, aLayers := blobsOf(t, a)

	if err := remote.Delete(e.ref(testProject + "/images/app@" + digestOf(t, a).String())); err != nil {
		t.Fatalf("registry DELETE: %v", err)
	}

	if e.hasBlob(aCfg) || e.hasBlob(aLayers[1]) {
		t.Error("A's config or unshared layer is still on disk")
	}
	if !e.hasBlob(aLayers[0]) {
		t.Error("the layer shared with B was removed")
	}
	e.pullAll("app:b")
}

// A layer that another repository also holds keeps its file.
func TestDeleteVersionKeepsLayerSharedWithAnotherRepository(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	e.createRepo(testLoc, "other")
	shared := randomLayer(t)
	a := imageOf(t, "a", shared)
	e.push("app:a", a)
	if err := remote.Write(e.ref(testProject+"/other/app:x"), imageOf(t, "x", shared)); err != nil {
		t.Fatal(err)
	}
	_, layers := blobsOf(t, a)

	e.deleteVersion("app", digestOf(t, a))

	if !e.hasBlob(layers[0]) {
		t.Fatal("a layer the other repository uses was removed")
	}
	if _, err := remote.Image(e.ref(testProject + "/other/app:x")); err != nil {
		t.Fatal(err)
	}
}

// Deleting an index leaves its children and their blobs; deleting a child
// afterwards frees the child's blobs.
func TestDeleteIndexKeepsChildren(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(e.ref(testProject+"/images/multi:v1"), idx); err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	child, err := idx.Image(im.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	cfg, layers := blobsOf(t, child)

	e.deleteVersion("multi", digestOf(t, idx))

	for _, c := range im.Manifests {
		if _, err := remote.Image(e.ref(testProject + "/images/multi@" + c.Digest.String())); err != nil {
			t.Fatalf("child %s after deleting the index: %v", c.Digest, err)
		}
	}
	if !e.hasBlob(cfg) || !e.hasBlob(layers[0]) {
		t.Fatal("deleting the index removed a child's blobs")
	}

	e.deleteVersion("multi", im.Manifests[0].Digest)

	if e.hasBlob(cfg) || e.hasBlob(layers[0]) {
		t.Fatal("deleting the child left its blobs")
	}
}

// Deleting a tag frees nothing.
func TestDeleteTagFreesNothing(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	a := imageOf(t, "a", randomLayer(t))
	e.push("app:a", a)
	cfg, layers := blobsOf(t, a)
	if err := remote.Delete(e.ref(testProject + "/images/app:a")); err != nil {
		t.Fatalf("registry DELETE of the tag: %v", err)
	}
	if !e.hasBlob(cfg) || !e.hasBlob(layers[0]) || !e.hasBlob(digestOf(t, a)) {
		t.Fatal("deleting a tag removed blobs")
	}
	e.pullAll("app@" + digestOf(t, a).String())
}

// strayUpload leaves an upload file with no session in dir's registry, as
// a restart in the middle of a push does, and returns its path.
func strayUpload(t *testing.T, dir string) string {
	t.Helper()
	uploads := filepath.Join(dir, "data", "ar", "uploads")
	if err := os.MkdirAll(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(uploads, "left-by-a-restart")
	if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Outside a Deterministic Instance a pass runs at start.
func TestCleanupPassRunsAtStart(t *testing.T) {
	dir := t.TempDir()
	stray := strayUpload(t, dir)
	start(t, emutest.WithTestDataDir(t, dir))
	deadline := time.Now().Add(10 * time.Second)
	for exists(stray) {
		if time.Now().After(deadline) {
			t.Fatal("the upload file with no session is still there 10s after start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// In a Deterministic Instance no pass runs at start or as real time
// passes; advancing the Emulator clock runs one before it returns.
func TestDeterministicCleanupPassOnlyOnClockAdvance(t *testing.T) {
	dir := t.TempDir()
	stray := strayUpload(t, dir)
	e := start(t, emutest.WithTestDataDir(t, dir), emutest.WithDeterministic())
	e.createRepo(testLoc, "images")
	l := randomLayer(t)
	d := digestOf(t, l)
	if err := remote.WriteLayer(e.ref(testProject+"/images/app").Context(), l); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)
	if !exists(stray) {
		t.Fatal("a pass ran at start in a Deterministic Instance")
	}

	if err := e.inst.Internal().AdvanceClock(e.ctx, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(stray) {
		t.Fatal("advancing the clock left the upload file with no session")
	}
	if e.hasBlob(d) {
		t.Fatal("advancing the clock 2h left a layer no manifest uses")
	}
}

// Pushes, version deletes sharing a layer with them, and cleanup passes
// run at once; every version left afterwards pulls in full. A push that
// raced the delete of its shared layer's last user may fail, as on any
// registry, and is pushed again.
func TestCleanupPassConcurrentWithPushesAndDeletes(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	shared := randomLayer(t)
	const rounds = 15
	var keeps, drops []v1.Image
	for i := range rounds {
		keeps = append(keeps, imageOf(t, fmt.Sprintf("keep-%d", i), shared, randomLayer(t)))
		drops = append(drops, imageOf(t, fmt.Sprintf("drop-%d", i), shared, randomLayer(t)))
	}
	// raced reports a push that lost its shared layer to a concurrent
	// delete, the one failure ADR 0070 allows.
	raced := func(err error) bool { return err != nil && strings.Contains(err.Error(), "MANIFEST_BLOB_UNKNOWN") }
	var wg sync.WaitGroup
	errs := make(chan error, 3*rounds)
	wg.Add(3)
	go func() { // keeps pushing images that use the shared layer
		defer wg.Done()
		for i, img := range keeps {
			err := remote.Write(e.ref(fmt.Sprintf("%s/images/app:keep-%d", testProject, i)), img)
			for raced(err) {
				err = remote.Write(e.ref(fmt.Sprintf("%s/images/app:keep-%d", testProject, i)), img)
			}
			if err != nil {
				errs <- fmt.Errorf("push keep-%d: %w", i, err)
			}
		}
	}()
	go func() { // pushes and deletes images that use it too
		defer wg.Done()
		for i, img := range drops {
			if err := remote.Write(e.ref(fmt.Sprintf("%s/images/app:drop-%d", testProject, i)), img); raced(err) {
				continue
			} else if err != nil {
				errs <- fmt.Errorf("push drop-%d: %w", i, err)
				continue
			}
			d, err := img.Digest()
			if err != nil {
				errs <- err
				continue
			}
			if err := remote.Delete(e.ref(testProject + "/images/app@" + d.String())); err != nil {
				errs <- fmt.Errorf("delete drop-%d: %w", i, err)
			}
		}
	}()
	go func() { // and passes run meanwhile, past the grace period
		defer wg.Done()
		for range rounds {
			if err := e.inst.Internal().AdvanceClock(e.ctx, 2*time.Hour); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if err := e.inst.Internal().AdvanceClock(e.ctx, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	tags, err := remote.List(e.ref(testProject + "/images/app").Context())
	if err != nil {
		t.Fatal(err)
	}
	keep := 0
	for _, tag := range tags {
		if strings.HasPrefix(tag, "keep-") {
			keep++
		}
		e.pullAll("app:" + tag)
	}
	if keep != rounds {
		t.Fatalf("%d keep- tags, want %d", keep, rounds)
	}
}

// A push in progress across a pass (layers uploaded, manifest not yet put)
// succeeds, and the image pulls.
func TestPushInProgressAcrossPass(t *testing.T) {
	e := start(t, emutest.WithDeterministic())
	e.createRepo(testLoc, "images")
	l := randomLayer(t)
	if err := remote.WriteLayer(e.ref(testProject+"/images/app").Context(), l); err != nil {
		t.Fatal(err)
	}
	if err := e.inst.Internal().AdvanceClock(e.ctx, 59*time.Minute); err != nil {
		t.Fatal(err)
	}
	e.push("app:v1", imageOf(t, "v1", l))
	e.pullAll("app:v1")
}
