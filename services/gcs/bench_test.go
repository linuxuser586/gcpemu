package gcs_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/linuxuser586/gcpemu/emutest"
)

// BenchmarkThroughput measures single-stream upload and download through
// the official client (FR-GCS-010: target >= 200 MB/s on local SSD).
func BenchmarkThroughput(b *testing.B) {
	const size = 256 << 20
	inst := emutest.Start(b, []string{"gcs"})
	b.Setenv("STORAGE_EMULATOR_HOST", inst.Endpoint("gcs"))
	ctx := context.Background()
	c, err := storage.NewClient(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	bk := c.Bucket("bench")
	if err := bk.Create(ctx, testProject, nil); err != nil {
		b.Fatal(err)
	}
	data := randBytes(size, 1)
	b.Run("upload", func(b *testing.B) {
		b.SetBytes(size)
		for i := 0; i < b.N; i++ {
			w := bk.Object("big").NewWriter(ctx)
			w.ChunkSize = 16 << 20
			if _, err := w.Write(data); err != nil {
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("download", func(b *testing.B) {
		b.SetBytes(size)
		for i := 0; i < b.N; i++ {
			r, err := bk.Object("big").NewReader(ctx)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, r); err != nil {
				b.Fatal(err)
			}
			r.Close()
		}
	})
}

// TestThroughputTarget asserts FR-GCS-010 (>= 200 MB/s single-stream
// upload and download) when GCPEMU_PERF_TESTS=1, on a quiet host.
func TestThroughputTarget(t *testing.T) {
	if os.Getenv("GCPEMU_PERF_TESTS") != "1" {
		t.Skip("set GCPEMU_PERF_TESTS=1 to check FR-GCS-010")
	}
	const size = 256 << 20
	inst := emutest.Start(t, []string{"gcs"})
	t.Setenv("STORAGE_EMULATOR_HOST", inst.Endpoint("gcs"))
	ctx := context.Background()
	c, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bk := c.Bucket("perf")
	if err := bk.Create(ctx, testProject, nil); err != nil {
		t.Fatal(err)
	}
	data := randBytes(size, 2)
	rate := func(d time.Duration) float64 { return float64(size) / d.Seconds() / 1e6 }
	start := time.Now()
	w := bk.Object("big").NewWriter(ctx)
	w.ChunkSize = 16 << 20
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	up := rate(time.Since(start))
	start = time.Now()
	r, err := bk.Object("big").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	r.Close()
	down := rate(time.Since(start))
	t.Logf("upload %.0f MB/s, download %.0f MB/s", up, down)
	if up < 200 || down < 200 {
		t.Errorf("FR-GCS-010: upload %.0f MB/s, download %.0f MB/s, want >= 200", up, down)
	}
}
