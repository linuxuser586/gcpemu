package gcs_test

import (
	"context"
	"io"
	"testing"

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
