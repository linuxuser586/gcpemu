package gcs_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/services/gcs"
)

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(seed))
	_, _ = r.Read(b)
	return b
}

func httpCode(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

func write(t *testing.T, ctx context.Context, o *storage.ObjectHandle, data []byte, chunk int) *storage.ObjectAttrs {
	t.Helper()
	w := o.NewWriter(ctx)
	w.ChunkSize = chunk
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close %s (%d bytes): %v", o.ObjectName(), len(data), err)
	}
	return w.Attrs()
}

func read(t *testing.T, ctx context.Context, o *storage.ObjectHandle) []byte {
	t.Helper()
	r, err := o.NewReader(ctx)
	if err != nil {
		t.Fatalf("reader %s: %v", o.ObjectName(), err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", o.ObjectName(), err)
	}
	return b
}

func TestBuckets(t *testing.T) {
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		b := c.Bucket("my-bucket")
		attrs := &storage.BucketAttrs{
			Location:                 "us-central1",
			Labels:                   map[string]string{"env": "dev", "team": "x"},
			VersioningEnabled:        true,
			UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
			CORS:                     []storage.CORS{{MaxAge: time.Hour, Methods: []string{"GET"}, Origins: []string{"*"}, ResponseHeaders: []string{"Content-Type"}}},
			Website:                  &storage.BucketWebsite{MainPageSuffix: "index.html", NotFoundPage: "404.html"},
			RetentionPolicy:          &storage.RetentionPolicy{RetentionPeriod: time.Hour},
			DefaultEventBasedHold:    false,
			StorageClass:             "NEARLINE",
		}
		if err := b.Create(ctx, testProject, attrs); err != nil {
			t.Fatal(err)
		}
		got, err := b.Attrs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Location != "US-CENTRAL1" || got.LocationType != "region" || got.StorageClass != "NEARLINE" {
			t.Errorf("location/class = %s %s %s", got.Location, got.LocationType, got.StorageClass)
		}
		if !got.VersioningEnabled || !got.UniformBucketLevelAccess.Enabled || got.UniformBucketLevelAccess.LockedTime.IsZero() {
			t.Errorf("versioning/ubla = %v %+v", got.VersioningEnabled, got.UniformBucketLevelAccess)
		}
		if got.Labels["env"] != "dev" || len(got.CORS) != 1 || got.Website.MainPageSuffix != "index.html" {
			t.Errorf("labels/cors/website = %v %v %v", got.Labels, got.CORS, got.Website)
		}
		if got.RetentionPolicy == nil || got.RetentionPolicy.RetentionPeriod != time.Hour || got.RetentionPolicy.EffectiveTime.IsZero() {
			t.Errorf("retention = %+v", got.RetentionPolicy)
		}
		if got.MetaGeneration != 1 || got.ProjectNumber == 0 || got.Created.IsZero() {
			t.Errorf("metageneration/project/created = %d %d %v", got.MetaGeneration, got.ProjectNumber, got.Created)
		}

		// Update: label add/delete, versioning off, retention removed.
		ua := storage.BucketAttrsToUpdate{VersioningEnabled: false, RetentionPolicy: &storage.RetentionPolicy{}}
		ua.SetLabel("new", "v")
		ua.DeleteLabel("team")
		up, err := b.If(storage.BucketConditions{MetagenerationMatch: 1}).Update(ctx, ua)
		if err != nil {
			t.Fatal(err)
		}
		if up.VersioningEnabled || up.Labels["new"] != "v" || up.Labels["team"] != "" || up.Labels["env"] != "dev" || up.MetaGeneration != 2 || up.RetentionPolicy != nil {
			t.Errorf("after update: %+v", up)
		}
		if _, err := b.If(storage.BucketConditions{MetagenerationMatch: 1}).Update(ctx, ua); httpCode(err) != http.StatusPreconditionFailed {
			t.Errorf("stale metageneration update err = %v", err)
		}

		// Name rules, uniqueness, listing.
		if err := c.Bucket("Bad_Name").Create(ctx, testProject, nil); httpCode(err) != http.StatusBadRequest {
			t.Errorf("invalid name err = %v", err)
		}
		if err := c.Bucket("goog-bucket").Create(ctx, testProject, nil); httpCode(err) != http.StatusBadRequest {
			t.Errorf("goog prefix err = %v", err)
		}
		if err := c.Bucket("x-bucket").Create(ctx, testProject, &storage.BucketAttrs{Location: "mars-west1"}); httpCode(err) != http.StatusBadRequest {
			t.Errorf("invalid location err = %v", err)
		}
		if err := b.Create(ctx, testProject, nil); httpCode(err) != http.StatusConflict {
			t.Errorf("duplicate err = %v", err)
		}
		if err := c.Bucket("other-bucket").Create(ctx, "other-project", nil); err != nil {
			t.Fatal(err)
		}
		var names []string
		it := c.Buckets(ctx, testProject)
		for {
			ba, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, ba.Name)
		}
		if len(names) != 1 || names[0] != "my-bucket" {
			t.Errorf("list = %v", names)
		}

		// Non-empty delete is refused (FR-CORE-026).
		write(t, ctx, b.Object("o"), []byte("x"), 0)
		if err := b.Delete(ctx); httpCode(err) != http.StatusConflict {
			t.Errorf("non-empty delete err = %v", err)
		}
		if err := b.Object("o").Delete(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.Delete(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
			t.Errorf("after delete err = %v", err)
		}
	})
}

func TestUploadDownload(t *testing.T) {
	const chunk = 256 << 10
	sizes := []int{0, 1, 1000, chunk - 1, chunk, chunk + 1, 3*chunk + 17}
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		b := c.Bucket("uploads")
		if err := b.Create(ctx, testProject, nil); err != nil {
			t.Fatal(err)
		}
		for i, n := range sizes {
			for _, cs := range []int{0, chunk} {
				data := randBytes(n, int64(i))
				o := b.Object("dir/obj-" + string(rune('a'+i)))
				attrs := write(t, ctx, o, data, cs)
				sum := md5.Sum(data)
				if attrs.Size != int64(n) || !bytes.Equal(attrs.MD5, sum[:]) || attrs.CRC32C != crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)) {
					t.Errorf("size %d chunk %d: attrs size=%d md5=%x crc=%d", n, cs, attrs.Size, attrs.MD5, attrs.CRC32C)
				}
				if got := read(t, ctx, o); !bytes.Equal(got, data) {
					t.Errorf("size %d chunk %d: read %d bytes, mismatch", n, cs, len(got))
				}
			}
		}
		// Metadata round trip.
		o := b.Object("meta.txt")
		w := o.NewWriter(ctx)
		w.ContentType = "text/plain"
		w.Metadata = map[string]string{"k": "v"}
		w.CacheControl = "no-cache"
		_, _ = w.Write([]byte("hello"))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		a, err := o.Attrs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a.ContentType != "text/plain" || a.Metadata["k"] != "v" || a.CacheControl != "no-cache" || a.Generation == 0 || a.Metageneration != 1 {
			t.Errorf("attrs = %+v", a)
		}
		r, err := o.NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Attrs.ContentType != "text/plain" || r.Attrs.Size != 5 || r.Attrs.Generation != a.Generation {
			t.Errorf("reader attrs = %+v", r.Attrs)
		}
		r.Close()
		if _, err := b.Object("missing").NewReader(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("missing object err = %v", err)
		}
	})
}

func TestRangedReads(t *testing.T) {
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		b := c.Bucket("ranges")
		if err := b.Create(ctx, testProject, nil); err != nil {
			t.Fatal(err)
		}
		data := randBytes(100_000, 7)
		o := b.Object("r")
		write(t, ctx, o, data, 0)
		for _, tc := range []struct {
			off, n   int64
			from, to int
		}{
			{0, 10, 0, 10},
			{500, 1000, 500, 1500},
			{99_990, -1, 99_990, 100_000},
			{99_990, 100, 99_990, 100_000},
			{-100, -1, 99_900, 100_000},
			{1, 0, 1, 1},
		} {
			r, err := o.NewRangeReader(ctx, tc.off, tc.n)
			if err != nil {
				t.Fatalf("range %d/%d: %v", tc.off, tc.n, err)
			}
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatalf("range %d/%d read: %v", tc.off, tc.n, err)
			}
			if !bytes.Equal(got, data[tc.from:tc.to]) {
				t.Errorf("range %d/%d: got %d bytes, want %d", tc.off, tc.n, len(got), tc.to-tc.from)
			}
			if tc.n != 0 && r.Attrs.Size != int64(len(data)) {
				t.Errorf("range %d/%d: size %d", tc.off, tc.n, r.Attrs.Size)
			}
		}
	})
}

func TestPreconditions(t *testing.T) {
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		b := c.Bucket("conds")
		if err := b.Create(ctx, testProject, nil); err != nil {
			t.Fatal(err)
		}
		o := b.Object("x")
		a1 := writeCond(t, ctx, o.If(storage.Conditions{DoesNotExist: true}), []byte("one"), nil)
		writeCond(t, ctx, o.If(storage.Conditions{DoesNotExist: true}), []byte("two"), func(err error) bool { return httpCode(err) == 412 })
		writeCond(t, ctx, o.If(storage.Conditions{GenerationMatch: a1.Generation + 1}), []byte("two"), func(err error) bool { return httpCode(err) == 412 })
		a2 := writeCond(t, ctx, o.If(storage.Conditions{GenerationMatch: a1.Generation}), []byte("two"), nil)
		if a2.Generation <= a1.Generation {
			t.Errorf("generation did not increase: %d -> %d", a1.Generation, a2.Generation)
		}
		if _, err := o.If(storage.Conditions{MetagenerationMatch: 5}).Update(ctx, storage.ObjectAttrsToUpdate{ContentType: "a/b"}); httpCode(err) != 412 {
			t.Errorf("metageneration mismatch update err = %v", err)
		}
		up, err := o.If(storage.Conditions{MetagenerationMatch: 1}).Update(ctx, storage.ObjectAttrsToUpdate{ContentType: "a/b", Metadata: map[string]string{"x": "y"}})
		if err != nil {
			t.Fatal(err)
		}
		if up.Metageneration != 2 || up.ContentType != "a/b" || up.Metadata["x"] != "y" {
			t.Errorf("update = %+v", up)
		}
		if _, err := o.If(storage.Conditions{GenerationMatch: a1.Generation}).NewReader(ctx); httpCode(err) != 412 {
			t.Errorf("read with stale generation err = %v", err)
		}
		if err := o.If(storage.Conditions{GenerationNotMatch: a2.Generation}).Delete(ctx); httpCode(err) != 412 {
			t.Errorf("delete GenerationNotMatch err = %v", err)
		}
		if err := o.If(storage.Conditions{GenerationMatch: a2.Generation}).Delete(ctx); err != nil {
			t.Errorf("delete: %v", err)
		}
	})
}

func writeCond(t *testing.T, ctx context.Context, o *storage.ObjectHandle, data []byte, wantErr func(error) bool) *storage.ObjectAttrs {
	t.Helper()
	w := o.NewWriter(ctx)
	_, _ = w.Write(data)
	err := w.Close()
	switch {
	case wantErr == nil && err != nil:
		t.Fatalf("write: %v", err)
	case wantErr != nil && !wantErr(err):
		t.Fatalf("write: unexpected error %v", err)
	}
	return w.Attrs()
}

func TestVersioning(t *testing.T) {
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		b := c.Bucket("versions")
		if err := b.Create(ctx, testProject, &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
			t.Fatal(err)
		}
		o := b.Object("v")
		a1 := write(t, ctx, o, []byte("v1"), 0)
		a2 := write(t, ctx, o, []byte("v2"), 0)
		if got := read(t, ctx, o.Generation(a1.Generation)); string(got) != "v1" {
			t.Errorf("old generation = %q", got)
		}
		if got := read(t, ctx, o); string(got) != "v2" {
			t.Errorf("live = %q", got)
		}
		if err := o.Delete(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("live after delete err = %v", err)
		}
		var gens []int64
		it := b.Objects(ctx, &storage.Query{Versions: true})
		for {
			a, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.Deleted.IsZero() {
				t.Errorf("generation %d not marked noncurrent", a.Generation)
			}
			gens = append(gens, a.Generation)
		}
		if len(gens) != 2 || gens[0] != a1.Generation || gens[1] != a2.Generation {
			t.Errorf("versions = %v, want [%d %d]", gens, a1.Generation, a2.Generation)
		}
		if got := read(t, ctx, o.Generation(a2.Generation)); string(got) != "v2" {
			t.Errorf("noncurrent read = %q", got)
		}
		if err := o.Generation(a1.Generation).Delete(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Generation(a1.Generation).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("permanently deleted generation err = %v", err)
		}
		if err := b.Delete(ctx); httpCode(err) != http.StatusConflict {
			t.Errorf("bucket with noncurrent versions delete err = %v", err)
		}
	})
}

func TestComposeCopyRewrite(t *testing.T) {
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		b := c.Bucket("compose")
		dst := c.Bucket("compose-dst")
		for _, bk := range []*storage.BucketHandle{b, dst} {
			if err := bk.Create(ctx, testProject, nil); err != nil {
				t.Fatal(err)
			}
		}
		parts := [][]byte{[]byte("alpha-"), []byte("beta-"), []byte("gamma")}
		var srcs []*storage.ObjectHandle
		for i, p := range parts {
			o := b.Object("part" + string(rune('0'+i)))
			write(t, ctx, o, p, 0)
			srcs = append(srcs, o)
		}
		comp := b.Object("composed")
		ca, err := comp.ComposerFrom(srcs...).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []byte("alpha-beta-gamma")
		if ca.ComponentCount != 3 || ca.Size != int64(len(want)) || ca.CRC32C != crc32.Checksum(want, crc32.MakeTable(crc32.Castagnoli)) {
			t.Errorf("compose attrs = %+v", ca)
		}
		if got := read(t, ctx, comp); !bytes.Equal(got, want) {
			t.Errorf("composed = %q", got)
		}

		// Copy across buckets with new metadata.
		cp := dst.Object("copy")
		copier := cp.CopierFrom(comp)
		copier.ContentType = "text/plain"
		cpa, err := copier.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cpa.ContentType != "text/plain" || cpa.CRC32C != ca.CRC32C {
			t.Errorf("copy attrs = %+v", cpa)
		}
		if got := read(t, ctx, cp); !bytes.Equal(got, want) {
			t.Errorf("copy = %q", got)
		}

		// Rewrite with a token loop.
		defer gcs.SetRewriteChunk(1 << 20)()
		big := randBytes(3<<20+5, 3)
		src := b.Object("big")
		write(t, ctx, src, big, 0)
		rw := dst.Object("big-copy").CopierFrom(src)
		calls := 0
		rw.ProgressFunc = func(copied, total uint64) { calls++ }
		if _, err := rw.Run(ctx); err != nil {
			t.Fatal(err)
		}
		if calls < 4 {
			t.Errorf("rewrite progress calls = %d, want >= 4", calls)
		}
		if got := read(t, ctx, dst.Object("big-copy")); !bytes.Equal(got, big) {
			t.Error("rewrite content mismatch")
		}
		// Source preconditions.
		cp2 := dst.Object("copy2").CopierFrom(src.If(storage.Conditions{GenerationMatch: 1}))
		if _, err := cp2.Run(ctx); httpCode(err) != 412 {
			t.Errorf("copy with failing source precondition err = %v", err)
		}
	})
}

func TestListDelimiter(t *testing.T) {
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		b := c.Bucket("listing")
		if err := b.Create(ctx, testProject, nil); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"a/1", "a/2", "a/b/3", "b/1", "c", "d.txt", "e.txt"} {
			write(t, ctx, b.Object(n), []byte(n), 0)
		}
		list := func(q *storage.Query, pageSize int) (objs, prefixes []string) {
			t.Helper()
			it := b.Objects(ctx, q)
			it.PageInfo().MaxSize = pageSize
			for {
				a, err := it.Next()
				if err == iterator.Done {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if a.Prefix != "" {
					prefixes = append(prefixes, a.Prefix)
				} else {
					objs = append(objs, a.Name)
				}
			}
		}
		for _, ps := range []int{1, 2, 1000} {
			objs, prefixes := list(&storage.Query{Delimiter: "/"}, ps)
			if !equal(objs, []string{"c", "d.txt", "e.txt"}) || !equal(prefixes, []string{"a/", "b/"}) {
				t.Errorf("page %d: objs=%v prefixes=%v", ps, objs, prefixes)
			}
		}
		objs, prefixes := list(&storage.Query{Prefix: "a/", Delimiter: "/"}, 1000)
		if !equal(objs, []string{"a/1", "a/2"}) || !equal(prefixes, []string{"a/b/"}) {
			t.Errorf("prefix a/: objs=%v prefixes=%v", objs, prefixes)
		}
		objs, _ = list(&storage.Query{MatchGlob: "**.txt"}, 1000)
		if !equal(objs, []string{"d.txt", "e.txt"}) {
			t.Errorf("glob: %v", objs)
		}
		objs, _ = list(&storage.Query{MatchGlob: "a/*"}, 1000)
		if !equal(objs, []string{"a/1", "a/2"}) {
			t.Errorf("glob a/*: %v", objs)
		}
		objs, _ = list(&storage.Query{StartOffset: "b", EndOffset: "d.txt"}, 1)
		if !equal(objs, []string{"b/1", "c"}) {
			t.Errorf("offsets: %v", objs)
		}
		objs, _ = list(nil, 3)
		if len(objs) != 7 || !sort.StringsAreSorted(objs) {
			t.Errorf("all: %v", objs)
		}
	})
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
