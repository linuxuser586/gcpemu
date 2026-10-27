package instance

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// TestExternal: an Instance delegates the APIs configured with --external,
// points their *_EMULATOR_HOST variables at the API gateway, and reports
// each External emulator and whether it answers, without its readiness
// depending on them (#623, ADR 0027).
func TestExternal(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := l.Addr().String()
	l.Close()

	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.External = map[string]string{"firestore": up.Addr().String(), "spanner": down}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(ctx)
	if err := in.WaitReady(ctx); err != nil {
		t.Fatalf("not ready with an External emulator down: %v", err)
	}
	gw := in.Env.Endpoints.Get("gateway")

	vars := in.EnvVars()
	for _, k := range []string{"FIRESTORE_EMULATOR_HOST", "SPANNER_EMULATOR_HOST"} {
		if vars[k] != gw {
			t.Errorf("%s = %q, want the API gateway %s", k, vars[k], gw)
		}
	}

	var got struct {
		External []struct {
			API       string   `json:"api"`
			Address   string   `json:"address"`
			Hosts     []string `json:"hosts"`
			Reachable bool     `json:"reachable"`
			Error     string   `json:"error"`
		} `json:"external"`
	}
	getJSON(t, "http://"+gw+"/_emu/v1/external", nil, &got)
	if len(got.External) != 2 {
		t.Fatalf("external = %+v, want firestore and spanner", got.External)
	}
	fs, sp := got.External[0], got.External[1]
	if fs.API != "firestore" || fs.Address != up.Addr().String() || !fs.Reachable || fs.Error != "" ||
		strings.Join(fs.Hosts, ",") != "firestore.googleapis.com" {
		t.Errorf("firestore = %+v, want it reachable at %s", fs, up.Addr())
	}
	if sp.API != "spanner" || sp.Address != down || sp.Reachable || sp.Error == "" {
		t.Errorf("spanner = %+v, want it unreachable at %s, saying why", sp, down)
	}
}

// TestExternalServed: an Instance refuses to start when --external names
// an API that one of its Services serves (#623).
func TestExternalServed(t *testing.T) {
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.External = map[string]string{"dns": "localhost:8053"}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = in.Start(ctx)
	defer in.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), `the "dns" Service serves the dns API`) {
		t.Errorf("Start = %v, want a refusal naming the dns Service", err)
	}
}

// TestExternalLAN: on a --gfe-bind address, a request for a Delegated
// API's hostname without credentials the Instance issued is refused, and
// never reaches the External emulator (#623, ADR 0021).
func TestExternalLAN(t *testing.T) {
	var reached atomic.Int32
	ext := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer ext.Close()

	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.Ports[frontend.LANEndpoint] = 0
	cfg.GFEBind = []string{"127.0.0.1"}
	cfg.External = map[string]string{"firestore": ext.Listener.Addr().String()}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(ctx)
	lan := in.Env.Endpoints.Get(frontend.LANEndpoint)
	if lan == "" {
		t.Fatal("the Google front end serves no --gfe-bind address")
	}
	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: in.Env.CA.Pool(), ServerName: "firestore.googleapis.com"},
	}}
	for _, authz := range []string{"", "Bearer owner"} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+lan+"/v1/projects/p/databases/(default)/documents/c/d", nil)
		req.Host = "firestore.googleapis.com"
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", authz, resp.StatusCode)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the External emulator got %d requests, want none", n)
	}
}
