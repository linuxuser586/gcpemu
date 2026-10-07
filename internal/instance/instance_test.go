package instance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/fault"
)

type fakeSvc struct{ started, stopped bool }

func (f *fakeSvc) Name() string { return "dns" }
func (f *fakeSvc) Register(r emu.Router) error {
	r.Mount("dns", []string{"dns.googleapis.com"}, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/missing" {
			apierr.Write(w, apierr.NotFound("nope"))
			return
		}
		_, _ = io.WriteString(w, string(emu.PrincipalFrom(req.Context()))+" "+req.URL.Path)
	}))
	return nil
}
func (f *fakeSvc) Start(context.Context) error { f.started = true; return nil }
func (f *fakeSvc) Stop(context.Context) error  { f.stopped = true; return nil }
func (f *fakeSvc) Ready() error                { return nil }

func TestInstanceLifecycle(t *testing.T) {
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	svc := &fakeSvc{}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	base := "http://" + in.Env.Endpoints.Get("gateway")

	get := func(url, host string) (int, string) {
		req, _ := http.NewRequest("GET", url, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(base+"/dns/dns/v1/projects", ""); code != 200 || body != "user:dev@example.com /dns/v1/projects" {
		t.Errorf("prefix route: %d %q", code, body)
	}
	if code, body := get(base+"/dns/v1/x", "dns.googleapis.com"); code != 200 || !strings.HasSuffix(body, " /dns/v1/x") {
		t.Errorf("host route: %d %q", code, body)
	}
	code, body := get(base+"/dns/missing", "")
	var e struct {
		Error struct {
			Code   int
			Status string
		}
	}
	_ = json.Unmarshal([]byte(body), &e)
	if code != 404 || e.Error.Status != "NOT_FOUND" || e.Error.Code != 404 {
		t.Errorf("error envelope: %d %s", code, body)
	}
	if code, _ := get(base+"/_emu/v1/ready", ""); code != 200 {
		t.Errorf("ready = %d", code)
	}
	// No service used the container runtime: no containers, and listing
	// them does not connect to it.
	if code, body := get(base+"/_emu/v1/containers", ""); code != 200 || strings.Join(strings.Fields(body), "") != `{"containers":[]}` {
		t.Errorf("containers = %d %s", code, body)
	}
	if n := len(in.Log.Entries("dns")); n != 3 {
		t.Errorf("request log has %d dns entries, want 3", n)
	}
	if err := in.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if !svc.started || !svc.stopped {
		t.Errorf("started=%v stopped=%v", svc.started, svc.stopped)
	}
}

func TestFaultInjection(t *testing.T) {
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(ctx)
	base := "http://" + in.Env.Endpoints.Get("gateway")
	if _, err := in.Faults().Add(fault.Rule{Service: "dns", Code: "UNAVAILABLE", Count: 1}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{503, 200} {
		resp, err := http.Get(base + "/dns/ok")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("request %d: status %d, want %d", i, resp.StatusCode, want)
		}
	}
	if _, err := in.Faults().Add(fault.Rule{Service: "dns", Drop: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(base + "/dns/ok"); err == nil {
		t.Error("expected dropped connection")
	}
}

// TestBindWarning is FR-CORE-044: widening --bind beyond loopback logs a
// warning; the loopback default does not.
func TestBindWarning(t *testing.T) {
	for _, tc := range []struct {
		bind string
		warn bool
	}{{"127.0.0.1", false}, {"0.0.0.0", true}, {"192.0.2.10", true}} {
		cfg := config.Defaults()
		cfg.Ephemeral = true
		cfg.DataDir = t.TempDir()
		cfg.Services = []string{"dns"}
		cfg.Bind = tc.bind
		var out strings.Builder
		in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, &out)
		if err != nil {
			t.Fatal(err)
		}
		_ = in.Shutdown(context.Background())
		got := strings.Contains(out.String(), "level=WARN") && strings.Contains(out.String(), "bind="+tc.bind)
		if got != tc.warn {
			t.Errorf("bind %s: warned=%v, want %v; log:\n%s", tc.bind, got, tc.warn, out.String())
		}
	}
}

// TestRequestLogEntries is FR-CORE-061: every API call is logged with
// method, principal, resource, status and latency, in JSON and text.
func TestRequestLogEntries(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		cfg := config.Defaults()
		cfg.Ephemeral = true
		cfg.DataDir = t.TempDir()
		cfg.Services = []string{"dns"}
		cfg.Ports["gateway"] = 0
		cfg.LogFormat = format
		svc := emu.Service(&noteSvc{})
		var out syncBuffer
		in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, &out)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := in.Start(ctx); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get("http://" + in.Env.Endpoints.Get("gateway") + "/dns/v1/projects/p/managedZones/z")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		_ = in.Shutdown(ctx)

		es := in.Log.Entries("dns")
		if len(es) != 1 {
			t.Fatalf("%s: %d entries", format, len(es))
		}
		e := es[0]
		if e.Method != "GET /v1/projects/p/managedZones/z" || e.Resource != "//dns.googleapis.com/projects/p/managedZones/z" ||
			e.Principal != "user:dev@example.com" || e.Status != 200 || e.Protocol != "http" {
			t.Errorf("%s: entry = %+v", format, e)
		}
		want := `"resource":"//dns.googleapis.com/projects/p/managedZones/z"`
		if format == "text" {
			want = "resource=//dns.googleapis.com/projects/p/managedZones/z"
		}
		if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "latencyMs") {
			t.Errorf("%s log lacks %s:\n%s", format, want, out.String())
		}
	}
}

// noteSvc checks one permission per request, as service modules do.
type noteSvc struct{ fakeSvc }

func (n *noteSvc) Register(r emu.Router) error {
	auth := emu.NewPolicyAuthorizer(config.IAMOff, nil)
	r.Mount("dns", nil, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = auth.Check(req.Context(), "dns.managedZones.get", "//dns.googleapis.com"+strings.TrimPrefix(req.URL.Path, "/v1"))
		_ = auth.Check(req.Context(), "dns.changes.list", "//dns.googleapis.com/other")
	}))
	return nil
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
