package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// Pause and Unpause freeze and thaw a container's processes, so a
// directory can be copied out of it at one point in time (#286).
func TestPauseUnpause(t *testing.T) {
	var got []string
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path[strings.Index(r.URL.Path, "/containers/"):])
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := context.Background()
	if err := cl.PauseContainer(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := cl.UnpauseContainer(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /containers/c1/pause", "POST /containers/c1/unpause"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for n, d := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(d)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, d)
	}
	_ = tw.Close()
	return b.Bytes()
}

// ArchiveFrom streams a whole directory out of a container as the
// runtime's tar, entries named under the directory's base name.
func TestArchiveFrom(t *testing.T) {
	want := tarOf(t, map[string]string{"server/db/state.db": "data"})
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/containers/c1/archive") || r.URL.Query().Get("path") != "/var/lib/rancher/k3s/server" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(want)
	})
	var got bytes.Buffer
	if err := cl.ArchiveFrom(context.Background(), "c1", "/var/lib/rancher/k3s/server", &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("archive differs: %d bytes, want %d", got.Len(), len(want))
	}
	if err := cl.ArchiveFrom(context.Background(), "c1", "/missing", io.Discard); err == nil {
		t.Error("missing path: no error")
	}
}

// ArchiveTo extracts a tar stream into a directory of a container, which
// may be created but not started.
func TestArchiveTo(t *testing.T) {
	want := tarOf(t, map[string]string{"server/token": "t"})
	var got []byte
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/containers/c1/archive") || r.URL.Query().Get("path") != "/var/lib/rancher/k3s" ||
			r.Header.Get("Content-Type") != "application/x-tar" {
			w.WriteHeader(404)
			return
		}
		got, _ = io.ReadAll(r.Body)
	})
	if err := cl.ArchiveTo(context.Background(), "c1", "/var/lib/rancher/k3s", bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("uploaded %d bytes, want %d", len(got), len(want))
	}
}
