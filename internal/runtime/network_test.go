package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
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

// TestPinnedNetwork: a pinned network is created again on the subnet the
// runtime picked; when another network takes that subnet in between, it
// picks again, and when that keeps happening the network stays unpinned.
func TestPinnedNetwork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steals int // pinned creates refused with an overlap
		want   []string
	}{
		{"pinned", 0, []string{"", "172.18.0.0/16"}},
		{"retried", 1, []string{"", "172.18.0.0/16", "", "172.19.0.0/16"}},
		{"unpinned", 100, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var creates []string // requested subnet of each create
			next, steals := 18, tc.steals
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/networks/create"):
					var body struct {
						IPAM *struct{ Config []map[string]string }
					}
					b, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(b, &body)
					subnet := ""
					if body.IPAM != nil {
						subnet = body.IPAM.Config[0]["Subnet"]
					}
					creates = append(creates, subnet)
					if subnet != "" && steals > 0 {
						steals--
						next++
						w.WriteHeader(403)
						_, _ = io.WriteString(w, `{"message":"invalid pool request: Pool overlaps with other one on this address space"}`)
						return
					}
					_, _ = io.WriteString(w, `{"Id":"n1"}`)
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/n1"):
					_, _ = io.WriteString(w, `{"Id":"n1","Name":"x","IPAM":{"Config":[{"Subnet":"172.`+strconv.Itoa(next)+`.0.0/16","Gateway":"172.`+strconv.Itoa(next)+`.0.1"}]}}`)
				case r.Method == "DELETE":
				default:
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cl.CreateNetwork(context.Background(), NetworkSpec{Name: "x", Pin: true}); err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if last := creates[len(creates)-1]; last != "" || len(creates) != 21 {
					t.Errorf("creates = %d, last subnet %q; want 21, the last unpinned", len(creates), last)
				}
				return
			}
			if strings.Join(creates, ",") != strings.Join(tc.want, ",") {
				t.Errorf("creates = %q; want %q", creates, tc.want)
			}
		})
	}
}
