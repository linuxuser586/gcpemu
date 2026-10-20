package emutest_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	dns "google.golang.org/api/dns/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestWithSeed: the Seed file is applied before the Instance is ready; its
// project is declared, so strict projects accept it.
func TestWithSeed(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	if err := os.WriteFile(seed, []byte(`projects: [seed-proj]
gcs:
  buckets:
    - name: seed-bucket
      project: seed-proj
`), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := emutest.Start(t, []string{"gcs"}, emutest.WithSeed(seed), emutest.WithStrictProjects())
	sc := inst.Storage(t)
	ctx := context.Background()
	if _, err := sc.Bucket("seed-bucket").Attrs(ctx); err != nil {
		t.Errorf("seeded bucket: %v", err)
	}
	if err := sc.Bucket("seed-bucket-2").Create(ctx, "seed-proj", nil); err != nil {
		t.Errorf("bucket in the seeded project: %v", err)
	}
}

// TestWithStrictProjects: a project nobody declared is refused rather than
// created on first use.
func TestWithStrictProjects(t *testing.T) {
	inst := emutest.Start(t, []string{"gcs"}, emutest.WithStrictProjects())
	err := inst.Storage(t).Bucket("strict-bucket").Create(context.Background(), "undeclared-proj", nil)
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusNotFound {
		t.Fatalf("bucket in an undeclared project: %v, want 404", err)
	}
}

// TestWithDataDir: a Persistent Instance keeps its state in dir, which a
// later Instance in dir finds, and Close leaves dir in place.
func TestWithDataDir(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first, err := emutest.New(ctx, []string{"gcs"}, emutest.WithDataDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Storage(t).Bucket("kept-bucket").Create(ctx, "persist-proj", nil); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatalf("state in the data dir after Close: %v", err)
	}
	second := emutest.Start(t, []string{"gcs"}, emutest.WithDataDir(dir))
	if _, err := second.Storage(t).Bucket("kept-bucket").Attrs(ctx); err != nil {
		t.Errorf("bucket after restart: %v", err)
	}
}

// TestWithOperationLatency: a DNS change stays pending for the configured
// latency, then completes.
func TestWithOperationLatency(t *testing.T) {
	const latency = 300 * time.Millisecond
	inst := emutest.Start(t, []string{"dns"}, emutest.WithOperationLatency("dns", latency))
	opts, err := inst.HTTPOptions("dns")
	if err != nil {
		t.Fatal(err)
	}
	c, err := dns.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ManagedZones.Create("lat-proj", &dns.ManagedZone{Name: "z", DnsName: "z.test.", Description: "d"}).Do(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ch, err := c.Changes.Create("lat-proj", "z", &dns.Change{Additions: []*dns.ResourceRecordSet{
		{Name: "a.z.test.", Type: "A", Ttl: 60, Rrdatas: []string{"192.0.2.1"}},
	}}).Do()
	if err != nil || ch.Status != "pending" {
		t.Fatalf("change = %+v, %v; want pending", ch, err)
	}
	for ch.Status != "done" {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("change still %s after 10s", ch.Status)
		}
		time.Sleep(20 * time.Millisecond)
		if ch, err = c.Changes.Get("lat-proj", "z", ch.Id).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < latency {
		t.Errorf("change done after %v, want at least %v", d, latency)
	}
}

// TestWithOffline: the Services see an offline configuration.
func TestWithOffline(t *testing.T) {
	inst := emutest.Start(t, []string{"gcs"}, emutest.WithOffline())
	if !inst.Internal().Env.Config.Offline {
		t.Error("Env.Config.Offline = false")
	}
}

// TestNewClose: an Instance from New serves until Close, which removes
// its temporary data dir.
func TestNewClose(t *testing.T) {
	ctx := context.Background()
	inst, err := emutest.New(ctx, []string{"gcs"})
	if err != nil {
		t.Fatal(err)
	}
	dir := inst.Internal().Dir
	sc, err := storage.NewClient(ctx, append(mustHTTP(t, inst, "storage"), storage.WithJSONReads())...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if err := sc.Bucket("new-bucket").Create(ctx, "new-proj", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data dir while running: %v", err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data dir %s after Close: %v, want it gone", dir, err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func mustHTTP(t *testing.T, inst *emutest.Instance, api string) []option.ClientOption {
	t.Helper()
	opts, err := inst.HTTPOptions(api)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}
