package ar

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// passService is a service with a blob store and an Emulator clock the
// test moves, on which the Registry cleanup pass is run directly (#1045).
type passService struct {
	*Service
	t     *testing.T
	clock *clock.Offset
	ref   repoRef
}

func newPassService(t *testing.T) *passService {
	t.Helper()
	cfg := config.Defaults()
	c := clock.NewOffset(clock.Real{})
	env := &emu.Env{
		Config: &cfg, Store: store.NewMemory(), Clock: c, IDs: emu.NewIDs(false),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Endpoints: emu.NewEndpoints(),
	}
	s := New(env).(*Service)
	var err error
	if s.blobs, err = newBlobStore(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	p := &passService{Service: s, t: t, clock: c, ref: repoRef{"p", "us-central1", "r"}}
	err = env.Store.Update(func(tx store.Tx) error {
		return putRepo(tx, p.ref, &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER})
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// blob stores content and links it into img, as an upload does.
func (p *passService) blob(img, content string) string {
	p.t.Helper()
	d, n, err := p.blobs.put(bytes.NewReader([]byte(content)), "")
	if err != nil {
		p.t.Fatal(err)
	}
	err = p.env.Store.Update(func(tx store.Tx) error { return putLink(tx, p.ref, img, d, n, p.env.Clock.Now()) })
	if err != nil {
		p.t.Fatal(err)
	}
	return d
}

func (p *passService) linked(img, d string) bool {
	var ok bool
	_ = p.env.Store.View(func(tx store.Tx) error { _, ok = getLink(tx, p.ref, img, d); return nil })
	return ok
}

func (p *passService) pass() {
	p.t.Helper()
	if err := p.cleanupPass(context.Background()); err != nil {
		p.t.Fatalf("cleanup pass: %v", err)
	}
}

// A link no manifest uses survives a pass while younger than the grace
// period, and is dropped with its blob file once older.
func TestPassDropsOrphanLinkAfterGrace(t *testing.T) {
	p := newPassService(t)
	d := p.blob("app", "layer")

	p.clock.Advance(59 * time.Minute)
	p.pass()
	if !p.linked("app", d) || !p.blobs.has(d) {
		t.Fatal("a link younger than the grace period was dropped")
	}

	p.clock.Advance(2 * time.Minute)
	p.pass()
	if p.linked("app", d) {
		t.Fatal("an orphan link older than the grace period survived")
	}
	if p.blobs.has(d) {
		t.Fatal("the blob file of the dropped link survived")
	}
}

// A link a manifest of its image uses is kept however old; the same blob
// linked into another image without a manifest is dropped, and the file
// stays for the image that uses it.
func TestPassKeepsLinksAManifestUses(t *testing.T) {
	p := newPassService(t)
	d := p.blob("app", "layer")
	p.blob("other", "layer")
	err := p.env.Store.Update(func(tx store.Tx) error {
		return putManifest(tx, p.ref, "app", &manifestRec{Digest: "sha256:" + string(bytes.Repeat([]byte("a"), 64)), Blobs: []string{d}})
	})
	if err != nil {
		t.Fatal(err)
	}

	p.clock.Advance(2 * time.Hour)
	p.pass()

	if !p.linked("app", d) {
		t.Fatal("a link a manifest uses was dropped")
	}
	if p.linked("other", d) {
		t.Fatal("the orphan link in another image survived")
	}
	if !p.blobs.has(d) {
		t.Fatal("the blob file of a used link was removed")
	}
}

// startUpload opens an upload session in img, as a POST does.
func (p *passService) startUpload(img string) *upload {
	p.t.Helper()
	w := httptest.NewRecorder()
	p.Service.startUpload(w, httptest.NewRequest(http.MethodPost, "/", nil), &target{ref: p.ref, img: img, name: "p/r/" + img})
	if w.Code != http.StatusAccepted {
		p.t.Fatalf("start upload: %d %s", w.Code, w.Body)
	}
	id := w.Header().Get("Docker-Upload-UUID")
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.uploads[id]
}

func (p *passService) hasUpload(u *upload) bool {
	p.mu.Lock()
	_, live := p.uploads[u.id]
	p.mu.Unlock()
	_, err := os.Stat(p.blobs.uploadPath(u.id))
	return live && err == nil
}

// An upload file with no session is removed; a session untouched for
// longer than the grace period goes with its file; one that a chunk
// touched within it stays.
func TestPassDropsAbandonedUploads(t *testing.T) {
	p := newPassService(t)
	stray := p.blobs.uploadPath("left-by-a-restart")
	if err := os.WriteFile(stray, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	idle := p.startUpload("app")
	active := p.startUpload("app")

	p.clock.Advance(50 * time.Minute)
	active.mu.Lock()
	if err := p.appendUpload(active, strings.NewReader("chunk")); err != nil {
		t.Fatal(err)
	}
	active.mu.Unlock()
	p.clock.Advance(20 * time.Minute)
	p.pass()

	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("upload file with no session: stat = %v, want not exist", err)
	}
	if p.hasUpload(idle) {
		t.Error("a session idle for longer than the grace period survived")
	}
	if !p.hasUpload(active) {
		t.Error("a session touched within the grace period was dropped")
	}
}

// A push that uploads a blob again, whose old link no manifest uses, makes
// the link young again: the pass before its manifest arrives keeps it.
func TestPassKeepsLinkAPushRenewed(t *testing.T) {
	p := newPassService(t)
	d := p.blob("app", "layer")
	p.clock.Advance(2 * time.Hour)
	p.blob("app", "layer") // the next push uploads the layer again

	p.pass()

	if !p.linked("app", d) || !p.blobs.has(d) {
		t.Fatal("the pass dropped a link a push had just renewed")
	}
}

// Passes, uploads and Resets run at once without a data race, and a
// Reset returns only after the pass in progress (run with -race).
func TestPassConcurrentWithReset(t *testing.T) {
	p := newPassService(t)
	p.ready = true
	p.blob("app", "layer")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			p.clock.Advance(2 * time.Hour)
			_ = p.cleanupPass(context.Background())
		}
	}()
	for range 20 {
		p.startUpload("app")
		if err := p.Reset(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.ResetDone()
	}
	<-done
}

// A push that finds a blob already there (HEAD) skips uploading it; that
// makes an old link young again, as an upload would, so a pass before its
// manifest arrives keeps it.
func TestPassKeepsLinkAPushFoundByHead(t *testing.T) {
	p := newPassService(t)
	d := p.blob("app", "layer")
	p.clock.Advance(2 * time.Hour)
	w := httptest.NewRecorder()
	p.getBlob(w, httptest.NewRequest(http.MethodHead, "/", nil), &target{ref: p.ref, img: "app", name: "p/r/app"}, d)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", w.Code)
	}

	p.pass()

	if !p.linked("app", d) || !p.blobs.has(d) {
		t.Fatal("the pass dropped a link a push had just found by HEAD")
	}
}
