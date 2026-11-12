package instance

import (
	"context"
	"net"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// infoSvc contributes an entry to /_emu/v1/info under its Name (#1065).
type infoSvc struct{ fakeSvc }

func (s *infoSvc) Info() any { return map[string]int{"answer": 42} }

// TestServiceInfo: a service that implements emu.Informer has its entry in
// /_emu/v1/info under its own name; one that doesn't has no key there.
func TestServiceInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  emu.Service
		want bool
	}{
		{"informer", &infoSvc{}, true},
		{"plain", &fakeSvc{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Ephemeral = true
			cfg.DataDir = t.TempDir()
			cfg.Services = []string{"dns"}
			cfg.Ports["gateway"] = 0
			in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return tc.svc }}, &syncBuffer{})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := in.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer in.Shutdown(ctx)
			_, port, _ := net.SplitHostPort(in.Env.Endpoints.Get("gateway"))
			var info map[string]any
			getJSON(t, "http://127.0.0.1:"+port+"/_emu/v1/info", nil, &info)
			dns, ok := info["dns"]
			if ok != tc.want {
				t.Fatalf("info has dns = %v, want %v: %v", ok, tc.want, info)
			}
			if ok {
				if m, _ := dns.(map[string]any); m["answer"] != float64(42) {
					t.Errorf("info.dns = %v", dns)
				}
			}
			if info["instance"] != cfg.Instance {
				t.Errorf("info.instance = %v", info["instance"])
			}
		})
	}
}
