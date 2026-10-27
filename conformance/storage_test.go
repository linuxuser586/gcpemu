//go:build conformance

package conformance

import (
	"context"
	"io"
	"testing"

	"cloud.google.com/go/storage"
)

func TestStorageBuckets(t *testing.T) { storageBuckets.test(t) }
func TestStorageObjects(t *testing.T) { storageObjects.test(t) }

// storageClient is a Cloud Storage client for c, reading through the JSON
// API, closed when the test ends.
func storageClient(t testing.TB, c *conn) *storage.Client {
	t.Helper()
	cl, err := storage.NewClient(c.ctx, append(c.HTTP("storage"), storage.WithJSONReads())...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// createBucket creates the bucket base, deleted with its objects when the
// test ends.
func createBucket(t testing.TB, c *conn, cl *storage.Client, base string) *storage.BucketHandle {
	t.Helper()
	b := cl.Bucket(c.Name(base))
	if err := b.Create(c.ctx, c.Project(), &storage.BucketAttrs{Location: "US", Labels: c.Labels()}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	c.Cleanup(func(ctx context.Context) { _ = deleteBucket(ctx, b) })
	return b
}

// deleteBucket deletes every object in b and then b, returning the
// bucket's deletion error.
func deleteBucket(ctx context.Context, b *storage.BucketHandle) error {
	it := b.Objects(ctx, &storage.Query{Versions: true})
	for {
		o, err := it.Next()
		if err != nil {
			break
		}
		_ = b.Object(o.Name).Generation(o.Generation).Delete(ctx)
	}
	return b.Delete(ctx)
}

var storageBuckets = scenario{
	product: "storage",
	opts:    []Option{RunOnly("items")},
	body: func(t testing.TB, c *conn) {
		cl := storageClient(t, c)
		b := createBucket(t, c, cl, "bucket")
		if _, err := b.Attrs(c.ctx); err != nil {
			t.Fatalf("get: %v", err)
		}
		c.Retry(func() error {
			it := cl.Buckets(c.ctx, c.Project())
			it.Prefix = c.Name("")
			return listed(it.Next, "the new bucket", func(x *storage.BucketAttrs) bool { return x.Name == c.Name("bucket") })
		})
		var update storage.BucketAttrsToUpdate
		update.SetLabel("color", "blue")
		if _, err := b.Update(c.ctx, update); err != nil {
			t.Fatalf("update: %v", err)
		}
		if _, err := cl.Bucket(c.Name("missing")).Attrs(c.ctx); err == nil {
			t.Error("get of a missing bucket succeeded")
		}
		if err := b.Create(c.ctx, c.Project(), &storage.BucketAttrs{Location: "US"}); err == nil {
			t.Error("creating a bucket whose name is taken succeeded")
		}
		if err := b.Delete(c.ctx); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}

var storageObjects = scenario{
	product: "storage",
	opts:    []Option{RunOnly("items")},
	body: func(t testing.TB, c *conn) {
		cl := storageClient(t, c)
		b := createBucket(t, c, cl, "objects")
		o := b.Object("greeting.txt")
		write := func(o *storage.ObjectHandle, data string) error {
			w := o.NewWriter(c.ctx)
			w.ChunkSize = 0 // one multipart request: a resumable session's ID is random
			w.ContentType = "text/plain"
			if _, err := io.WriteString(w, data); err != nil {
				return err
			}
			return w.Close()
		}
		if err := write(o, "hello conformance\n"); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := o.Attrs(c.ctx); err != nil {
			t.Fatalf("get: %v", err)
		}
		r, err := o.NewReader(c.ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		_, _ = io.ReadAll(r)
		_ = r.Close()
		c.Retry(func() error {
			return listed(b.Objects(c.ctx, nil).Next, "the new object", func(x *storage.ObjectAttrs) bool { return x.Name == o.ObjectName() })
		})
		if _, err := o.Update(c.ctx, storage.ObjectAttrsToUpdate{Metadata: map[string]string{"color": "blue"}}); err != nil {
			t.Fatalf("update: %v", err)
		}
		if _, err := b.Object("missing.txt").Attrs(c.ctx); err == nil {
			t.Error("get of a missing object succeeded")
		}
		if err := write(o.If(storage.Conditions{DoesNotExist: true}), "again\n"); err == nil {
			t.Error("a write with ifGenerationMatch=0 over an existing object succeeded")
		}
		if err := o.Delete(c.ctx); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}
