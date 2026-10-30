package gke

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestServerFilesResetServingCert (#886, ADR 0051): the files copied into
// a Cluster server on every start (bringUpServer copies the same list
// whether or not again is set) hold an empty dynamic-cert.json, so k3s
// issues a serving certificate for the current --tls-san from its first
// handshake.
func TestServerFilesResetServingCert(t *testing.T) {
	const want = "var/lib/rancher/k3s/server/tls/dynamic-cert.json"
	found := false
	for _, f := range serverFiles("http://gw/hooks") {
		if f.Name != want {
			continue
		}
		found = true
		if len(f.Data) != 0 {
			t.Errorf("%s holds %q; want it empty", want, f.Data)
		}
	}
	if !found {
		t.Errorf("the pre-start files lack %s", want)
	}
}

// TestPrepareServerAfterRestore (#886): restoring a State snapshot writes
// the snapshot's server/ directory, its serving certificate included,
// into a new Cluster server; the empty dynamic-cert.json still wins, and
// the restored datastore is kept.
func TestPrepareServerAfterRestore(t *testing.T) {
	const (
		cert = "/var/lib/rancher/k3s/server/tls/dynamic-cert.json"
		db   = "/var/lib/rancher/k3s/server/db/state.db"
	)
	var mu sync.Mutex
	fsys := map[string]string{} // the container's files, as the archives leave them
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/containers/cp1/archive") {
			t.Errorf("unexpected runtime request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		tr := tar.NewReader(r.Body)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(tr)
			mu.Lock()
			fsys[path.Join(r.URL.Query().Get("path"), h.Name)] = string(b)
			mu.Unlock()
		}
	}))
	defer docker.Close()
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(docker.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &runtime.Manager{Client: cl, InstanceID: "i1"}

	staged := filepath.Join(t.TempDir(), "c1.tar.gz")
	f, err := os.Create(staged)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for name, data := range map[string]string{"server/tls/dynamic-cert.json": `{"stale":true}`, "server/db/state.db": "kine"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(data))
	}
	if err := errors.Join(tw.Close(), zw.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}

	if err := prepareServer(context.Background(), rt, "cp1", staged, serverFiles("http://gw/hooks")); err != nil {
		t.Fatal(err)
	}
	if got, ok := fsys[cert]; !ok || got != "" {
		t.Errorf("%s = %q (present %v); want it empty", cert, got, ok)
	}
	if got := fsys[db]; got != "kine" {
		t.Errorf("%s = %q; want the restored datastore kept", db, got)
	}
}

// fakeCertServer serves TLS with a certificate from a fresh cluster CA
// naming stale for the first staleFor handshakes and fresh after. It
// returns its address and the CA's PEM.
func fakeCertServer(t *testing.T, staleFor int32, stale, fresh []string) (string, []byte) {
	t.Helper()
	clusterCA, err := ca.Load(t.TempDir(), "cluster")
	if err != nil {
		t.Fatal(err)
	}
	var handshakes atomic.Int32
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if handshakes.Add(1) <= staleFor {
			return clusterCA.ServerCert(stale...)
		}
		return clusterCA.ServerCert(fresh...)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()
	return ln.Addr().String(), clusterCA.PEM()
}

// TestAwaitReadyPublicEndpointCert (#886): a ready API server whose
// certificate doesn't name the Public endpoint keeps the cluster from
// RUNNING; once the wait runs out the start fails with a status message
// naming the endpoint and the names the certificate covers. Served once
// it names the endpoint, the cluster goes through.
func TestAwaitReadyPublicEndpointCert(t *testing.T) {
	const pub = "203.0.113.7"
	stale := []string{"10.0.0.5", "127.0.0.1"}
	dial := (&net.Dialer{}).DialContext
	rt := fakeServerRuntime(t, "cp1", "running")
	ready := func() bool { return true }

	t.Run("never names it", func(t *testing.T) {
		addr, caPEM := fakeCertServer(t, math.MaxInt32, stale, nil) // never reissued
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		err := awaitReady(ctx, rt, "cp1", ready, func(ctx context.Context) error {
			return checkServedName(ctx, dial, addr, caPEM, pub)
		})
		if err == nil {
			t.Fatal("awaitReady succeeded; want the stale certificate to fail the start")
		}
		msg := apierr.From(err).Message // the cluster's ERROR status message (markError)
		for _, want := range []string{"Public endpoint", pub, "10.0.0.5", "127.0.0.1"} {
			if !strings.Contains(msg, want) {
				t.Errorf("status message %q; want it to contain %q", msg, want)
			}
		}
	})

	t.Run("names it after a reissue", func(t *testing.T) {
		addr, caPEM := fakeCertServer(t, 2, stale, append(slices.Clone(stale), pub))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := awaitReady(ctx, rt, "cp1", ready, func(ctx context.Context) error {
			return checkServedName(ctx, dial, addr, caPEM, pub)
		})
		if err != nil {
			t.Fatalf("awaitReady = %v; want the reissued certificate to let the cluster through", err)
		}
	})

	t.Run("private cluster", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := awaitReady(ctx, rt, "cp1", ready, nil); err != nil {
			t.Fatalf("awaitReady with no Public endpoint = %v; want nil", err)
		}
	})
}
