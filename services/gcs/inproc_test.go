package gcs_test

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// objectStore returns the running gcs Service as other Services see it.
func objectStore(t *testing.T, env *emu.Env) emu.ObjectStore {
	t.Helper()
	svc, ok := env.Lookup("gcs")
	if !ok {
		t.Fatal("gcs not running")
	}
	os, ok := svc.(emu.ObjectStore)
	if !ok {
		t.Fatal("gcs is not an emu.ObjectStore")
	}
	return os
}

func readAll(t *testing.T, store emu.ObjectStore, bucket, name string, gen int64) (string, int64) {
	t.Helper()
	rc, g, err := store.ReadObject(context.Background(), bucket, name, gen)
	if err != nil {
		t.Fatalf("ReadObject %s/%s#%d: %v", bucket, name, gen, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), g
}

// A Service creates a bucket whose name the user-facing rule refuses,
// appends to an object in it generation by generation, and reads any
// generation back; a generation that is gone answers NOT_FOUND.
func TestObjectStoreInProcess(t *testing.T) {
	inst := start(t)
	store := objectStore(t, inst.Env)
	ctx := context.Background()
	const bucket = "123.cloudbuild-logs.googleusercontent.com"
	for range 2 {
		if err := store.EnsureServiceBucket(ctx, testProject, bucket); err != nil {
			t.Fatalf("EnsureServiceBucket: %v", err)
		}
	}
	if code := httpStatus(t, http.MethodPost, inst.GatewayURL()+"/storage/v1/b?project="+testProject, `{"name":"other.googleusercontent.com"}`); code != http.StatusBadRequest {
		t.Errorf("user create of a google bucket = %d, want 400", code)
	}

	if err := store.AppendObject(ctx, bucket, "dir/log.txt", "text/plain", []byte("one\n")); err != nil {
		t.Fatalf("AppendObject: %v", err)
	}
	got, first := readAll(t, store, bucket, "dir/log.txt", 0)
	if got != "one\n" {
		t.Errorf("after one append = %q", got)
	}
	if err := store.AppendObject(ctx, bucket, "dir/log.txt", "text/plain", []byte("two\n")); err != nil {
		t.Fatalf("AppendObject: %v", err)
	}
	got, second := readAll(t, store, bucket, "dir/log.txt", 0)
	if got != "one\ntwo\n" || second <= first {
		t.Errorf("after two appends = %q, generation %d after %d", got, second, first)
	}
	if got, _ := readAll(t, store, bucket, "dir/log.txt", second); got != "one\ntwo\n" {
		t.Errorf("by generation = %q", got)
	}
	if _, _, err := store.ReadObject(ctx, bucket, "dir/log.txt", first); status.Code(err) != codes.NotFound {
		t.Errorf("stale generation: err = %v, want NOT_FOUND", err)
	}
	if _, _, err := store.ReadObject(ctx, bucket, "missing", 0); status.Code(err) != codes.NotFound {
		t.Errorf("missing object: err = %v, want NOT_FOUND", err)
	}
	if err := store.AppendObject(ctx, "no-such-bucket", "x", "", []byte("x")); status.Code(err) != codes.NotFound {
		t.Errorf("append to a missing bucket: err = %v, want NOT_FOUND", err)
	}
}

// A Service writes a whole object, as Cloud Build uploads an artifact:
// writing it again replaces it with a new generation, and the MD5 it
// reports is the data's.
func TestWriteObjectInProcess(t *testing.T) {
	inst := start(t)
	store := objectStore(t, inst.Env)
	ctx := context.Background()
	const bucket = "artifacts"
	if err := store.EnsureServiceBucket(ctx, testProject, bucket); err != nil {
		t.Fatal(err)
	}
	first, _, err := store.WriteObject(ctx, bucket, "bin/app", "", strings.NewReader("old"))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	gen, sum, err := store.WriteObject(ctx, bucket, "bin/app", "", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	// MD5("hello"), from md5sum.
	if got := hex.EncodeToString(sum); got != "5d41402abc4b2a76b9719d911017c592" {
		t.Errorf("md5 = %s", got)
	}
	if got, g := readAll(t, store, bucket, "bin/app", 0); got != "hello" || g != gen || gen <= first {
		t.Errorf("object = %q generation %d, want \"hello\" generation %d after %d", got, g, gen, first)
	}
	if _, _, err := store.WriteObject(ctx, "no-such-bucket", "x", "", strings.NewReader("x")); status.Code(err) != codes.NotFound {
		t.Errorf("write to a missing bucket: err = %v, want NOT_FOUND", err)
	}
}

// httpStatus sends a request and returns its status code.
func httpStatus(t *testing.T, method, url, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// gcloud tails a Build log by polling the object with Range: bytes=N-:
// 206 with the new bytes while it has grown past N, 416 at its end.
func TestRangedReadsOfAGrowingObject(t *testing.T) {
	inst := start(t)
	store := objectStore(t, inst.Env)
	ctx := context.Background()
	const bucket = "123.cloudbuild-logs.googleusercontent.com"
	if err := store.EnsureServiceBucket(ctx, testProject, bucket); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendObject(ctx, bucket, "log-1.txt", "text/plain", []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	read := func(from int) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, inst.GatewayURL()+"/"+bucket+"/log-1.txt", nil)
		req.Header.Set("Range", "bytes="+strconv.Itoa(from)+"-")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := read(0); code != http.StatusPartialContent || body != "first\n" {
		t.Errorf("from 0 = %d %q", code, body)
	}
	if code, _ := read(6); code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("from the end = %d, want 416", code)
	}
	if err := store.AppendObject(ctx, bucket, "log-1.txt", "", []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if code, body := read(6); code != http.StatusPartialContent || body != "second\n" {
		t.Errorf("from 6 after growing = %d %q", code, body)
	}
}
