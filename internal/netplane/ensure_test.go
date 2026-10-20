package netplane_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// fakeRaced is a Docker API on which another creator makes the network
// named raced between the plane's inspect and its create: the first
// inspect of raced finds nothing, its create gets createStatus, and later
// inspects find it with gateway.
func fakeRaced(t *testing.T, raced string, createStatus int, createMsg, gateway string) *runtime.Manager {
	t.Helper()
	var mu sync.Mutex
	exists := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/networks/create"):
			var body struct{ Name string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			if body.Name == raced {
				exists = true
				w.WriteHeader(createStatus)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": createMsg})
				return
			}
			_, _ = io.WriteString(w, `{"Id":"probe"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/probe"):
			_, _ = io.WriteString(w, `{"Id":"probe","IPAM":{"Config":[{"Subnet":"127.0.0.0/8","Gateway":"127.0.0.1"}]}}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/"+raced) && exists:
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "raced", "Name": raced,
				"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": "127.0.0.0/8", "Gateway": gateway}}}})
		case r.Method == "DELETE":
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(vmrelay.Env, "off")
	return &runtime.Manager{Client: cl, InstanceID: "i1", Ephemeral: true, Log: slog.Default()}
}

// TestEnsureAdoptsExisting: when the services or the external network's
// create says it already exists, the plane uses the network it finds by
// name (#588).
func TestEnsureAdoptsExisting(t *testing.T) {
	for _, tc := range []struct {
		suffix string
		get    func(*netplane.Plane, context.Context) (netplane.Net, error)
	}{
		{"svc", (*netplane.Plane).Services},
		{"ext", (*netplane.Plane).External},
	} {
		t.Run(tc.suffix, func(t *testing.T) {
			m := fakeRaced(t, "gcpemu-i1-"+tc.suffix, http.StatusConflict, "network with name gcpemu-i1-"+tc.suffix+" already exists", "127.0.0.1")
			p := netplane.New(m, func() map[string]string { return nil }, slog.Default())
			defer p.Close()
			n, err := tc.get(p, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n.Name != "gcpemu-i1-"+tc.suffix || n.Gateway != "127.0.0.1" {
				t.Errorf("net = %+v; want gcpemu-i1-%s with gateway 127.0.0.1", n, tc.suffix)
			}
		})
	}
}

// TestEnsureAdoptedNeedsGateway: a network adopted after "already exists"
// must have an IPv4 gateway, like one found by the first inspect.
func TestEnsureAdoptedNeedsGateway(t *testing.T) {
	m := fakeRaced(t, "gcpemu-i1-svc", http.StatusConflict, "network with name gcpemu-i1-svc already exists", "")
	p := netplane.New(m, func() map[string]string { return nil }, slog.Default())
	defer p.Close()
	if _, err := p.Services(context.Background()); err == nil || !strings.Contains(err.Error(), "has no IPv4 gateway") {
		t.Errorf("Services: %v; want the no-IPv4-gateway error", err)
	}
}

// TestEnsureOtherCreateError: create errors other than "already exists"
// are returned as they are.
func TestEnsureOtherCreateError(t *testing.T) {
	m := fakeRaced(t, "gcpemu-i1-svc", http.StatusInternalServerError, "disk full", "127.0.0.1")
	p := netplane.New(m, func() map[string]string { return nil }, slog.Default())
	defer p.Close()
	if _, err := p.Services(context.Background()); err == nil || !strings.Contains(err.Error(), "runtime API 500: disk full") {
		t.Errorf("Services: %v; want the create error", err)
	}
}
