package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/link"
)

// startLinked starts a Persistent Instance named name in dir, its link
// listener on loopback.
func startLinked(t *testing.T, name, dir string) *Instance {
	t.Helper()
	cfg := config.Defaults()
	cfg.Instance = name
	cfg.DataDir = dir
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.Ports[link.Endpoint] = 0
	cfg.LinkBind = []string{"127.0.0.1"}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(context.Background()) })
	return in
}

// callAdmin calls in's Admin API and decodes a 200 reply into out.
func callAdmin(t *testing.T, in *Instance, method, path string, body, out any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+in.Env.Endpoints.Get("gateway")+path, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
}

func linkNames(t *testing.T, in *Instance) []string {
	t.Helper()
	var got struct{ Links []link.LinkState }
	callAdmin(t, in, http.MethodGet, "/_emu/v1/links", nil, &got)
	out := []string{}
	for _, l := range got.Links {
		out = append(out, l.Name)
	}
	return out
}

// Two Instances pair through their Admin APIs, as `gcpemu link` drives
// them, and the Pairings are configuration: Reset and a restart of a
// Persistent Instance keep them (ADR 0027).
func TestLinkPairingThroughAdminAPI(t *testing.T) {
	laptopDir := t.TempDir()
	laptop, office := startLinked(t, "laptop", laptopDir), startLinked(t, "office", t.TempDir())
	addr := office.Env.Endpoints.Get(link.Endpoint)
	if addr == "" {
		t.Fatal("no link endpoint")
	}

	var id link.Identity
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/links/identify", map[string]string{"address": addr}, &id)
	if id.Name != "office" || id.ID != office.ID {
		t.Fatalf("identify = %+v", id)
	}
	paired := make(chan link.Link, 1)
	go func() {
		var p link.Link
		callAdmin(t, laptop, http.MethodPost, "/_emu/v1/links", map[string]string{"address": addr, "fingerprint": id.Fingerprint}, &p)
		paired <- p
	}()
	var reqs struct{ Requests []link.Request }
	for i := 0; i < 200 && len(reqs.Requests) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		callAdmin(t, office, http.MethodGet, "/_emu/v1/links/requests", nil, &reqs)
	}
	if len(reqs.Requests) != 1 || reqs.Requests[0].InstanceID != laptop.ID {
		t.Fatalf("office's requests = %+v", reqs)
	}
	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/requests/"+reqs.Requests[0].ID+"/approve", map[string]string{}, nil)
	if p := <-paired; p.Name != "office" {
		t.Errorf("laptop paired with %+v", p)
	}

	var st struct{ Links []link.Status }
	callAdmin(t, laptop, http.MethodGet, "/_emu/v1/links/status", nil, &st)
	if len(st.Links) != 1 || !st.Links[0].Reachable || st.Links[0].Version != link.Current.Max {
		t.Errorf("laptop's link status = %+v", st)
	}

	callAdmin(t, office, http.MethodPost, "/_emu/v1/reset", nil, nil)
	if got := linkNames(t, office); len(got) != 1 || got[0] != "laptop" {
		t.Errorf("office's links after Reset = %v", got)
	}
	if err := laptop.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	laptop = startLinked(t, "laptop", laptopDir)
	if got := linkNames(t, laptop); len(got) != 1 || got[0] != "office" {
		t.Errorf("laptop's links after restart = %v", got)
	}

	callAdmin(t, laptop, http.MethodDelete, "/_emu/v1/links/office", nil, nil)
	if got := linkNames(t, laptop); len(got) != 0 {
		t.Errorf("links after unpair = %v", got)
	}
}
