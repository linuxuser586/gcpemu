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

// TestPinnedNetworkProbeName: the probe of a pinned create has a name of
// its own and the real network's labels, so the real name is created
// exactly once (#588).
func TestPinnedNetworkProbeName(t *testing.T) {
	var mu sync.Mutex
	type create struct {
		name, subnet string
		labels       map[string]string
	}
	var creates []create
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/networks/create"):
			var body struct {
				Name   string
				Labels map[string]string
				IPAM   *struct{ Config []map[string]string }
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			c := create{name: body.Name, labels: body.Labels}
			if body.IPAM != nil {
				c.subnet = body.IPAM.Config[0]["Subnet"]
			}
			creates = append(creates, c)
			_, _ = io.WriteString(w, `{"Id":"n`+strconv.Itoa(len(creates))+`"}`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/networks/n"):
			_, _ = io.WriteString(w, `{"Id":"n1","Name":"x","IPAM":{"Config":[{"Subnet":"172.18.0.0/16","Gateway":"172.18.0.1"}]}}`)
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
	labels := map[string]string{"gcpemu.instance": "i1"}
	if _, err := cl.CreateNetwork(context.Background(), NetworkSpec{Name: "gcpemu-i1-svc", Labels: labels, Pin: true}); err != nil {
		t.Fatal(err)
	}
	if len(creates) != 2 {
		t.Fatalf("creates = %+v; want a probe and the real network", creates)
	}
	probe, pinned := creates[0], creates[1]
	if probe.name == "gcpemu-i1-svc" || !strings.HasPrefix(probe.name, "gcpemu-i1-svc-probe-") || probe.labels["gcpemu.instance"] != "i1" {
		t.Errorf("probe = %+v; want its own name and the real network's labels", probe)
	}
	if pinned.name != "gcpemu-i1-svc" || pinned.subnet != "172.18.0.0/16" {
		t.Errorf("pinned = %+v; want gcpemu-i1-svc on 172.18.0.0/16", pinned)
	}
}

// TestPinnedNetworkFallbackProbe: with the default pools used up, the
// probe of a pinned create comes from the fallback range too, and the
// emulator's container must not join it, or the runtime refuses to delete
// it ("has active endpoints"). The emulator joins the real network once.
func TestPinnedNetworkFallbackProbe(t *testing.T) {
	var mu sync.Mutex
	subnets := map[string]string{} // network ID and name -> subnet
	var connects []string
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
			id := "n" + strconv.Itoa(len(subnets))
			subnets[id], subnets[body.Name] = body.IPAM.Config[0]["Subnet"], body.IPAM.Config[0]["Subnet"]
			_, _ = io.WriteString(w, `{"Id":"`+id+`"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/connect"):
			name := strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/networks/")+len("/networks/"):], "/connect")
			connects = append(connects, name)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/networks/"):
			key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			s, ok := subnets[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_, _ = io.WriteString(w, `{"Id":"`+key+`","Name":"`+key+`","IPAM":{"Config":[{"Subnet":"`+s+`"}]}}`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/containers/"):
			_, _ = io.WriteString(w, `{"NetworkSettings":{"Networks":{}}}`)
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
	cl.self = "self1"
	if _, err := cl.CreateNetwork(context.Background(), NetworkSpec{Name: "gcpemu-i1-svc", Pin: true}); err != nil {
		t.Fatal(err)
	}
	if len(connects) != 1 || connects[0] != "gcpemu-i1-svc" {
		t.Errorf("connects = %q; want the emulator on gcpemu-i1-svc once, never on the probe", connects)
	}
}
