package instance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/fault"
	"github.com/linuxuser586/gcpemu/internal/store"
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
	// A readiness hold (the startup seed) makes the instance not ready.
	release := in.HoldReady("seed", "applying seed.yaml")
	if code, body := get(base+"/_emu/v1/ready", ""); code != 503 || !strings.Contains(body, "applying seed.yaml") {
		t.Errorf("ready while held = %d %s", code, body)
	}
	release()
	if code, _ := get(base+"/_emu/v1/ready", ""); code != 200 || !in.Ready() {
		t.Errorf("ready after release = %d", code)
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
		cfg.LogFormat = "text" // CI=true defaults to JSON
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

// namedSvc is a fake service; failing makes Start return an error.
type namedSvc struct {
	name    string
	failing bool
	started bool
}

func (n *namedSvc) Name() string { return n.name }
func (n *namedSvc) Register(r emu.Router) error {
	r.Mount(n.name, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, n.name) }))
	return nil
}
func (n *namedSvc) Start(context.Context) error {
	if n.failing {
		return errors.New("no container runtime")
	}
	n.started = true
	return nil
}
func (n *namedSvc) Stop(context.Context) error { return nil }
func (n *namedSvc) Ready() error {
	if !n.started {
		return errors.New("not started")
	}
	return nil
}

// TestServiceIsolation is NFR-REL-003 and FR-CORE-003: a service that
// fails to start reports not-ready with its reason while the others serve;
// services that were not selected are never constructed.
func TestServiceIsolation(t *testing.T) {
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"gcs", "sql"}
	cfg.Ports[config.AllPorts] = 0
	built := map[string]*namedSvc{}
	factories := map[string]Factory{}
	for _, name := range config.AllServices {
		factories[name] = func(*emu.Env) emu.Service {
			s := &namedSvc{name: name, failing: name == "sql"}
			built[name] = s
			return s
		}
	}
	in, err := New(&cfg, factories, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(ctx)
	want := config.ResolveServices([]string{"gcs", "sql"})
	if len(built) != len(want) {
		t.Errorf("built %d services, want %v", len(built), want)
	}
	for _, name := range []string{"pubsub", "gke", "lb", "dns", "ar"} {
		if built[name] != nil {
			t.Errorf("unselected service %s was constructed", name)
		}
	}
	base := "http://" + in.Env.Endpoints.Get("gateway")
	resp, err := http.Get(base + "/_emu/v1/ready")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Ready    bool
		Services map[string]struct {
			Ready  bool
			Reason string
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if resp.StatusCode != 503 || r.Ready || r.Services["sql"].Ready || !strings.Contains(r.Services["sql"].Reason, "no container runtime") ||
		!r.Services["gcs"].Ready || !r.Services["iam"].Ready {
		t.Fatalf("ready = %d %+v", resp.StatusCode, r)
	}
	resp, err = http.Get(base + "/gcs/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "gcs" {
		t.Errorf("healthy service while another failed: %d %q", resp.StatusCode, b)
	}
}

type countingSvc struct{ fakeSvc }

func (c *countingSvc) ResourceCounts() map[string]int { return map[string]int{"managedZones": 2} }

// TestConsole is FR-UI-002 and FR-CI-003: the gateway serves the Web
// console unless it is turned off, under CI=true by default, or the bind
// is beyond loopback; and the admin API carries what its dashboard shows
// (FR-UI-010).
func TestConsole(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name    string
		ci      string
		console *bool
		bind    string
		served  bool
	}{
		{"default", "", nil, "127.0.0.1", true},
		{"--console=false", "", &off, "127.0.0.1", false},
		{"CI", "true", nil, "127.0.0.1", false},
		{"CI --console", "true", &on, "127.0.0.1", true},
		{"beyond loopback", "", nil, "0.0.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CI", tc.ci)
			cfg := config.Defaults()
			cfg.Ephemeral = true
			cfg.DataDir = t.TempDir()
			cfg.Services = []string{"dns"}
			cfg.Ports["gateway"] = 0
			cfg.Console = tc.console
			cfg.Bind = tc.bind
			cfg.LogFormat = "text"
			var log syncBuffer
			in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &countingSvc{} }}, &log)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := in.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer in.Shutdown(ctx)
			_, port, _ := net.SplitHostPort(in.Env.Endpoints.Get("gateway"))
			base := "http://127.0.0.1:" + port
			resp, err := http.Get(base + "/console/gcs/buckets?project=p1")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if served := resp.StatusCode == 200 && strings.Contains(string(b), "<html"); served != tc.served {
				t.Errorf("console served = %v (%d), want %v", served, resp.StatusCode, tc.served)
			}
			if tc.bind != "127.0.0.1" && !strings.Contains(log.String(), "Web console not served") {
				t.Errorf("no warning for bind %s:\n%s", tc.bind, log.String())
			}

			var info struct {
				Console bool
				Runtime struct {
					Kind      string
					Reachable bool
					Error     string
				}
			}
			getJSON(t, base+"/_emu/v1/info", &info)
			if info.Console != tc.served {
				t.Errorf("info.console = %v, want %v", info.Console, tc.served)
			}
			if info.Runtime.Kind == "" || (info.Runtime.Kind == "none") != (info.Runtime.Error != "") {
				t.Errorf("info.runtime = %+v", info.Runtime)
			}
			var counts map[string]map[string]int
			getJSON(t, base+"/_emu/v1/resources/counts", &counts)
			if counts["dns"]["managedZones"] != 2 {
				t.Errorf("counts = %v", counts)
			}
		})
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
}

// storeSvc writes a managed zone to the store on every request.
type storeSvc struct {
	fakeSvc
	env *emu.Env
}

func (s *storeSvc) Register(r emu.Router) error {
	r.Mount("dns", nil, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Put("dns/zones", "p/z", []byte(`{}`)) })
	}))
	return nil
}

// TestEvents: a Service's store write and the request that caused it reach
// /_emu/v1/events, and Shutdown ends the stream (FR-UI-006).
func TestEvents(t *testing.T) {
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	in, err := New(&cfg, map[string]Factory{"dns": func(env *emu.Env) emu.Service { return &storeSvc{env: env} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	base := "http://" + in.Env.Endpoints.Get("gateway")
	resp, err := http.Get(base + "/_emu/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	r, err := http.Get(base + "/dns/v1/projects/p/managedZones/z")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var seen []string
	for len(seen) < 2 && sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			seen = append(seen, data)
		}
	}
	if len(seen) < 2 || seen[0] != `{"service":"dns","namespace":"dns/zones","key":"p/z"}` ||
		!strings.Contains(seen[1], `"method":"GET /v1/projects/p/managedZones/z"`) {
		t.Errorf("events = %q", seen)
	}

	done := make(chan struct{})
	go func() {
		for sc.Scan() {
		}
		close(done)
	}()
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := in.Shutdown(sctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("event stream still open after Shutdown")
	}
}

// opsSvc is a Service named name holding ops.
type opsSvc struct {
	fakeSvc
	name string
	ops  []emu.OperationInfo
}

func (s *opsSvc) Name() string                    { return s.name }
func (s *opsSvc) Register(emu.Router) error       { return nil }
func (s *opsSvc) Operations() []emu.OperationInfo { return s.ops }

// TestOperations: /_emu/v1/operations lists every Service's Operations,
// newest first, filtered by Service and Project (FR-UI-012).
func TestOperations(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns", "compute", "gcs"}
	cfg.Ports["gateway"] = 0
	in, err := New(&cfg, map[string]Factory{
		"dns": func(*emu.Env) emu.Service {
			return &opsSvc{name: "dns", ops: []emu.OperationInfo{
				{Name: "projects/p1/managedZones/z/changes/1", Project: "p1", Done: true, StartTime: t0, EndTime: t0.Add(time.Second)},
			}}
		},
		"compute": func(*emu.Env) emu.Service {
			return &opsSvc{name: "compute", ops: []emu.OperationInfo{
				{Name: "projects/p2/global/operations/a", Project: "p2", StartTime: t0.Add(time.Minute)},
				{Name: "projects/p1/global/operations/b", Project: "p1", StartTime: t0.Add(-time.Minute),
					Done: true, Error: &emu.OperationError{Code: "RESOURCE_ALREADY_EXISTS", Message: "exists"}},
			}}
		},
		"gcs": func(*emu.Env) emu.Service { return &opsSvc{name: "gcs"} },
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(ctx)
	base := "http://" + in.Env.Endpoints.Get("gateway")

	names := func(query string) []string {
		var got struct{ Operations []emu.OperationInfo }
		getJSON(t, base+"/_emu/v1/operations"+query, &got)
		out := []string{}
		for _, op := range got.Operations {
			out = append(out, op.Service+" "+op.Name)
		}
		return out
	}
	for query, want := range map[string][]string{
		"": {
			"compute projects/p2/global/operations/a",
			"dns projects/p1/managedZones/z/changes/1",
			"compute projects/p1/global/operations/b",
		},
		"?service=dns":                {"dns projects/p1/managedZones/z/changes/1"},
		"?project=p1&service=compute": {"compute projects/p1/global/operations/b"},
		"?service=gcs":                {},
	} {
		if got := names(query); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("operations%s = %q, want %q", query, got, want)
		}
	}

	var raw struct{ Operations []map[string]any }
	getJSON(t, base+"/_emu/v1/operations?service=compute&project=p1", &raw)
	if op := raw.Operations[0]; op["done"] != true || op["endTime"] != nil ||
		op["error"].(map[string]any)["code"] != "RESOURCE_ALREADY_EXISTS" {
		t.Errorf("wire form = %v", op)
	}
}
