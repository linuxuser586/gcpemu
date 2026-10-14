package gke

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// fakeArchiver records the server container calls archiveServer makes.
type fakeArchiver struct {
	calls   []string
	copyErr error
}

func (f *fakeArchiver) PauseContainer(_ context.Context, id string) error {
	f.calls = append(f.calls, "pause "+id)
	return nil
}

func (f *fakeArchiver) UnpauseContainer(_ context.Context, id string) error {
	f.calls = append(f.calls, "unpause "+id)
	return nil
}

func (f *fakeArchiver) ArchiveFrom(_ context.Context, id, p string, w io.Writer) error {
	f.calls = append(f.calls, "archive "+id+":"+p)
	if f.copyErr != nil {
		_, _ = io.WriteString(w, "partial")
		return f.copyErr
	}
	_, _ = io.WriteString(w, "tar")
	return nil
}

// archiveServer copies only the server/ directory, with the container
// paused, into a gzip file named by the cluster ID (#286).
func TestArchiveServer(t *testing.T) {
	f := &fakeArchiver{}
	p := filepath.Join(t.TempDir(), "clusters", "123.tar.gz")
	if err := archiveServer(context.Background(), f, "cp", p); err != nil {
		t.Fatal(err)
	}
	want := "pause cp,archive cp:/var/lib/rancher/k3s/server,unpause cp"
	if got := strings.Join(f.calls, ","); got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
	r, err := openArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if b, _ := io.ReadAll(r); !bytes.Equal(b, []byte("tar")) {
		t.Errorf("archive = %q", b)
	}
}

// A failed copy still unpauses the server and leaves no archive behind.
func TestArchiveServerUnpausesOnFailure(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeArchiver{copyErr: boom}
	p := filepath.Join(t.TempDir(), "123.tar.gz")
	if err := archiveServer(context.Background(), f, "cp", p); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n := len(f.calls); n == 0 || f.calls[n-1] != "unpause cp" {
		t.Errorf("calls = %v, want unpause last", f.calls)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("archive left behind: %v", err)
	}
}

// snapshotService is a Service with no container runtime, so any attempt
// to reach a data plane fails.
func snapshotService(t *testing.T) *Service {
	t.Helper()
	cfg := config.Defaults()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints(), DataDir: t.TempDir()}).(*Service)
}

// Clusters that are not RUNNING are skipped, not failing the save.
func TestSnapshotSkipsClustersNotRunning(t *testing.T) {
	s := snapshotService(t)
	for i, st := range []containerpb.Cluster_Status{containerpb.Cluster_PROVISIONING, containerpb.Cluster_RECONCILING,
		containerpb.Cluster_STOPPING, containerpb.Cluster_ERROR} {
		name := string(rune('a' + i))
		rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: name}}
		rec.setCluster(&containerpb.Cluster{Name: name, Id: name, Status: st})
		if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if err := s.Snapshot(context.Background(), dir); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("saved %v", ents)
	}
	if len(s.busy) != 0 {
		t.Errorf("locks left: %v", s.busy)
	}
}

// Restore stages each saved archive as its cluster's datastore; Reset drops
// them.
func TestRestoreStagesDatastores(t *testing.T) {
	s := snapshotService(t)
	dir := t.TempDir()
	if err := os.MkdirAll(archivesDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archiveFile(archivesDir(dir), "abc"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if p, err := s.stagedDatastore("abc"); err != nil || p == "" {
		t.Fatalf("stagedDatastore(abc) = %q, %v", p, err)
	}
	if p, _ := s.stagedDatastore("other"); p != "" {
		t.Errorf("stagedDatastore(other) = %q", p)
	}
	_ = s.Reset(context.Background())
	if p, _ := s.stagedDatastore("abc"); p != "" {
		t.Errorf("after Reset: stagedDatastore(abc) = %q", p)
	}
}
