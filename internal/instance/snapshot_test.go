package instance

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// snapSvc keeps a file outside the store, as GCS keeps object data.
type snapSvc struct {
	fakeSvc
	env      *emu.Env
	ops      []emu.OperationInfo
	file     string
	churnKey string // a key of dns/churn to write on every Snapshot
	churns   int    // the value of the last churn write, so each one differs
	restored string
}

// IsStoreCache implements emu.StoreCacher: dns/churn keys under cache/
// are cache.
func (s *snapSvc) IsStoreCache(ns, key string) bool {
	return ns == "dns/churn" && strings.HasPrefix(key, "cache/")
}

func (s *snapSvc) Operations() []emu.OperationInfo { return s.ops }

func (s *snapSvc) Snapshot(_ context.Context, dir string) error {
	if s.churnKey != "" {
		s.churns++
		if err := s.env.Store.Update(func(tx store.Tx) error {
			return tx.Put("dns/churn", s.churnKey, []byte(strconv.Itoa(s.churns)))
		}); err != nil {
			return err
		}
	}
	return emu.CopyTree(filepath.Dir(s.file), dir)
}

func (s *snapSvc) Restore(_ context.Context, dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, filepath.Base(s.file)))
	s.restored = string(b)
	return err
}

func startSnapInstance(t *testing.T) (*Instance, *snapSvc) {
	t.Helper()
	cfg := testConfig()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	svc := &snapSvc{file: filepath.Join(t.TempDir(), "data")}
	in, err := New(&cfg, map[string]Factory{"dns": func(env *emu.Env) emu.Service { svc.env = env; return svc }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(context.Background()) })
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return in, svc
}

func TestSnapshotRoundTrip(t *testing.T) {
	in, svc := startSnapInstance(t)
	ctx := t.Context()
	put := func(v string) {
		if err := in.Env.Store.Update(func(tx store.Tx) error { return tx.Put("dns/zones", "z", []byte(v)) }); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(svc.file, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("one")
	if _, err := in.SaveSnapshot(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	put("two")
	if err := in.RestoreSnapshot(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	var got []byte
	_ = in.Env.Store.View(func(tx store.Tx) error { got, _ = tx.Get("dns/zones", "z"); return nil })
	if string(got) != "one" || svc.restored != "one" {
		t.Errorf("restored store %q, service file %q; want one", got, svc.restored)
	}
	snaps, err := in.Snapshots()
	if err != nil || len(snaps) != 1 || snaps[0].Name != "s1" || snaps[0].CreateTime.IsZero() {
		t.Errorf("Snapshots() = %+v, %v", snaps, err)
	}
	// An interrupted save leaves only a dot-directory, which is not listed.
	if err := os.Mkdir(filepath.Join(in.Dir, SnapshotsDir, ".save-123"), 0o700); err != nil {
		t.Fatal(err)
	}
	if snaps, _ := in.Snapshots(); len(snaps) != 1 {
		t.Errorf("Snapshots() lists a partial save: %+v", snaps)
	}
	if err := in.DeleteSnapshot("s1"); err != nil {
		t.Fatal(err)
	}
	if err := in.RestoreSnapshot(ctx, "s1"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("restore after delete: %v", err)
	}
}

func TestSnapshotBusy(t *testing.T) {
	in, svc := startSnapInstance(t)
	ctx := t.Context()

	svc.ops = []emu.OperationInfo{{Service: "dns", Name: "operations/1"}}
	if _, err := in.SaveSnapshot(ctx, "s"); !errors.Is(err, ErrSnapshotBusy) {
		t.Errorf("save with a running Operation: %v", err)
	}
	svc.ops[0].Done = true

	release := in.HoldReady("seed", "applying seed.yaml")
	if _, err := in.SaveSnapshot(ctx, "s"); !errors.Is(err, ErrSnapshotBusy) {
		t.Errorf("save while not ready: %v", err)
	}
	release()

	svc.churnKey = "state"
	if _, err := in.SaveSnapshot(ctx, "s"); !errors.Is(err, ErrSnapshotBusy) {
		t.Errorf("save while the store keeps changing: %v", err)
	}
	if snaps, _ := in.Snapshots(); len(snaps) != 0 {
		t.Errorf("failed saves left %+v", snaps)
	}
	svc.churnKey = ""
	if _, err := in.SaveSnapshot(ctx, "s"); err != nil {
		t.Errorf("save once quiet: %v", err)
	}
	for _, name := range []string{"", ".hidden", "a/b", "..", string(make([]byte, 64))} {
		if _, err := in.SaveSnapshot(ctx, name); !errors.Is(err, ErrSnapshotName) {
			t.Errorf("SaveSnapshot(%q): %v", name, err)
		}
	}
}

func TestSnapshotIgnoresCacheChurn(t *testing.T) {
	in, svc := startSnapInstance(t)
	ctx := t.Context()
	if err := os.WriteFile(svc.file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The first Snapshot writes the cache entry; every one after changes it.
	svc.churnKey = "cache/layer"
	if _, err := in.SaveSnapshot(ctx, "s"); err != nil {
		t.Fatalf("save while only cache entries change: %v", err)
	}
	get := func() string {
		var v []byte
		_ = in.Env.Store.View(func(tx store.Tx) error { v, _ = tx.Get("dns/churn", "cache/layer"); return nil })
		return string(v)
	}
	// The State snapshot holds the cache entry as the first Store dump had it.
	saved := get()
	if _, err := in.SaveSnapshot(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	svc.churnKey = ""
	if err := in.RestoreSnapshot(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if got := get(); saved == "" || got != saved {
		t.Errorf("restored cache entry %q; the State snapshot saved %q", got, saved)
	}
}
