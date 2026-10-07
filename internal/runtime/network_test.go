package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// TestNetworkPoolFallback: when the runtime's default address pools are
// used up, a network without a subnet is created on a free /24 of
// 100.64.0.0/10 that overlaps no existing network (FR-CORE-033).
func TestNetworkPoolFallback(t *testing.T) {
	var mu sync.Mutex
	existing := []string{"172.17.0.0/16", "100.64.0.0/11"} // the lower half of the fallback range is taken
	var created []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/networks/create"):
			var body struct {
				Name string
				IPAM *struct{ Config []map[string]string }
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			if body.IPAM == nil {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"message":"all predefined address pools have been fully subnetted"}`)
				return
			}
			cfg := body.IPAM.Config[0]
			p := netip.MustParsePrefix(cfg["Subnet"])
			for _, e := range existing {
				if netip.MustParsePrefix(e).Overlaps(p) {
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"message":"Pool overlaps with other one on this address space"}`)
					return
				}
			}
			existing = append(existing, cfg["Subnet"])
			created = append(created, cfg)
			_, _ = io.WriteString(w, `{"Id":"n1"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks"):
			var out []map[string]any
			for _, e := range existing {
				out = append(out, map[string]any{"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": e}}}})
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{http: srv.Client(), base: srv.URL + "/" + apiVersion, Endpoint: srv.URL}
	ctx := context.Background()
	for range 3 {
		if _, err := c.CreateNetwork(ctx, NetworkSpec{Name: "gcpemu-x-ext"}); err != nil {
			t.Fatal(err)
		}
	}
	fallback := netip.MustParsePrefix("100.64.0.0/10")
	seen := map[string]bool{}
	for _, cfg := range created {
		p := netip.MustParsePrefix(cfg["Subnet"])
		gw := netip.MustParseAddr(cfg["Gateway"])
		if p.Bits() != 24 || !fallback.Contains(p.Addr()) || netip.MustParsePrefix("100.64.0.0/11").Overlaps(p) || seen[p.String()] ||
			!p.Contains(gw) || gw.As4()[3] != 1 {
			t.Errorf("created %v", cfg)
		}
		seen[p.String()] = true
	}
	if len(created) != 3 {
		t.Fatalf("created %d networks", len(created))
	}

	// Other errors are returned as they are.
	if _, err := c.CreateNetwork(ctx, NetworkSpec{Name: "x", Subnet: "172.17.5.0/24"}); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("explicit overlapping subnet: %v", err)
	}
}
