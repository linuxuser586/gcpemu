package instance

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestShutdownWithAnEndedCtxAndAnOpenConnection is #1034: a connection to
// the API gateway still open when Teardown's ctx has ended is closed by
// force, not reported as context deadline exceeded, and nothing is left.
func TestShutdownWithAnEndedCtxAndAnOpenConnection(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Bind = "0.0.0.0" // so an Admin token file is written
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	svc := &scriptedSvc{name: "dns", journal: &journal{}}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	files := []string{PIDFile, EndpointsFile, AdminTokenFile}
	for _, name := range files {
		if _, err := os.Stat(filepath.Join(in.Dir, name)); err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
	}
	_, port, _ := net.SplitHostPort(in.Env.Endpoints.Get("gateway"))
	addr := "127.0.0.1:" + port
	// open sends no request, so the API gateway's HTTP server counts it as
	// active. A request on a later connection is answered only once open
	// has been accepted.
	open, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Get("http://" + addr + "/_emu/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := in.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	_ = open.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, open); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("connection to the API gateway still open after Shutdown")
	}
	for _, name := range files {
		if _, err := os.Stat(filepath.Join(in.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", name, err)
		}
	}
}
