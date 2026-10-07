package instance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
