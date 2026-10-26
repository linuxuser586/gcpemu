package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestReservedAddrs (#113): a container attached without a fixed address
// to a network whose subnet holds a reserved address gets the lowest
// address that is neither reserved, held by another container (running,
// or created with a fixed address) nor the gateway. Other networks are
// left to the runtime.
func TestReservedAddrs(t *testing.T) {
	var mu sync.Mutex
	var created []map[string]any // EndpointsConfig of each create
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/ext"):
			_, _ = io.WriteString(w, `{"Id":"e","Name":"ext","IPAM":{"Config":[{"Subnet":"172.24.0.0/16","Gateway":"172.24.0.1"}]},
				"Containers":{"a":{"IPv4Address":"172.24.0.3/16"}}}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/svc"):
			_, _ = io.WriteString(w, `{"Id":"s","Name":"svc","IPAM":{"Config":[{"Subnet":"10.9.0.0/24","Gateway":"10.9.0.1"}]}}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = io.WriteString(w, `[{"NetworkSettings":{"Networks":{"ext":{"IPAMConfig":{"IPv4Address":"172.24.0.4"}}}}}]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/containers/create"):
			var body struct {
				NetworkingConfig struct{ EndpointsConfig map[string]any }
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			created = append(created, body.NetworkingConfig.EndpointsConfig)
			_, _ = io.WriteString(w, `{"Id":"c1"}`)
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/networks/"):
			var body struct{ EndpointConfig map[string]any }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			net := strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/networks/")+len("/networks/"):], "/connect")
			created[len(created)-1][net] = body.EndpointConfig
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Client: cl, Reserved: func() []string { return []string{"172.24.0.2", "172.24.0.5", "10.1.2.3"} }}
	ip := func(ep any) string {
		cfg, _ := ep.(map[string]any)["IPAMConfig"].(map[string]any)
		s, _ := cfg["IPv4Address"].(string)
		return s
	}

	if _, err := m.CreateContainer(context.Background(), ContainerSpec{Name: "cp", Networks: []Attachment{{Network: "svc"}, {Network: "ext"}}}); err != nil {
		t.Fatal(err)
	}
	if got := ip(created[0]["ext"]); got != "172.24.0.6" {
		t.Errorf("ext address = %q; want 172.24.0.6 (.2 and .5 reserved, .3 and .4 held)", got)
	}
	if got := ip(created[0]["svc"]); got != "" {
		t.Errorf("svc address = %q; want none (no reservation in its subnet)", got)
	}

	// A fixed address is kept as asked, even a reserved one.
	if _, err := m.CreateContainer(context.Background(), ContainerSpec{Name: "sql", Networks: []Attachment{{Network: "ext", IP: "172.24.0.2"}}}); err != nil {
		t.Fatal(err)
	}
	if got := ip(created[1]["ext"]); got != "172.24.0.2" {
		t.Errorf("fixed address = %q; want 172.24.0.2", got)
	}
}

// fakeReserving serves network ext (172.24.0.0/16; .3 held by a running
// container, .4 by a created one) and records the address each create and
// connect asks for on it. It refuses the addresses in refuse as taken, as
// the runtime does for one held outside its view.
func fakeReserving(t *testing.T, refuse ...string) (*Manager, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body struct {
			NetworkingConfig struct {
				EndpointsConfig map[string]struct{ IPAMConfig struct{ IPv4Address string } }
			}
			EndpointConfig struct{ IPAMConfig struct{ IPv4Address string } }
		}
		ask := func(ip string) bool {
			asked = append(asked, ip)
			for _, a := range refuse {
				if ip == a {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"message":"Address already in use"}`)
					return false
				}
			}
			return true
		}
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/ext"):
			_, _ = io.WriteString(w, `{"Id":"e","Name":"ext","IPAM":{"Config":[{"Subnet":"172.24.0.0/16","Gateway":"172.24.0.1"}]},
				"Containers":{"a":{"IPv4Address":"172.24.0.3/16"}}}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = io.WriteString(w, `[{"NetworkSettings":{"Networks":{"ext":{"IPAMConfig":{"IPv4Address":"172.24.0.4"}}}}}]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/containers/create"):
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			if ask(body.NetworkingConfig.EndpointsConfig["ext"].IPAMConfig.IPv4Address) {
				_, _ = io.WriteString(w, `{"Id":"c1"}`)
			}
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/networks/ext/connect"):
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			ask(body.EndpointConfig.IPAMConfig.IPv4Address)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Client: cl, Reserved: func() []string { return []string{"172.24.0.2", "172.24.0.5"} }}
	return m, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// A container connected without a fixed address to a network whose subnet
// holds a reserved address gets the lowest free address outside the
// reserved set, as one created on it does (#673); a fixed address is kept.
func TestReservedAddrsConnect(t *testing.T) {
	m, asked := fakeReserving(t)
	if err := m.Connect(context.Background(), "ext", "edge", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Connect(context.Background(), "ext", "edge2", "172.24.0.9"); err != nil {
		t.Fatal(err)
	}
	if got, want := asked(), []string{"172.24.0.6", "172.24.0.9"}; !slices.Equal(got, want) {
		t.Errorf("connects asked for %q; want %q", got, want)
	}
}

// An address the runtime refuses as taken is replaced by the next free
// one outside the reserved set, never by the runtime's own choice, which
// could be a reserved address whose container has not started (#673).
func TestReservedAddrsRefused(t *testing.T) {
	m, asked := fakeReserving(t, "172.24.0.6")
	if _, err := m.CreateContainer(context.Background(), ContainerSpec{Name: "node", Networks: []Attachment{{Network: "ext"}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Connect(context.Background(), "ext", "edge", ""); err != nil {
		t.Fatal(err)
	}
	if got, want := asked(), []string{"172.24.0.6", "172.24.0.7", "172.24.0.6", "172.24.0.7"}; !slices.Equal(got, want) {
		t.Errorf("addresses asked for %q; want %q", got, want)
	}
}

// AddrHolder names the container holding an address on a network, whether
// it is attached with it or only created with it as its fixed address, and
// only on that network (ADR 0035).
func TestAddrHolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/containers/json") {
			_, _ = io.WriteString(w, `[
				{"Id":"run","Names":["/gke-node"],"Labels":{"gcpemu.instance":"i1"},
				 "NetworkSettings":{"Networks":{"ext":{"IPAddress":"172.24.0.3"},"svc":{"IPAddress":"10.9.0.5"}}}},
				{"Id":"made","Names":["/by-hand"],"Labels":{},
				 "NetworkSettings":{"Networks":{"ext":{"IPAMConfig":{"IPv4Address":"172.24.0.7"},"IPAddress":""}}}}]`)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		network, ip, id, name string
	}{
		{"ext", "172.24.0.3", "run", "gke-node"},
		{"ext", "172.24.0.7", "made", "by-hand"},
		{"ext", "10.9.0.5", "", ""}, // held on svc, not ext
		{"ext", "172.24.0.9", "", ""},
	} {
		h, ok, err := cl.AddrHolder(context.Background(), c.network, c.ip)
		if err != nil {
			t.Fatal(err)
		}
		if ok != (c.id != "") || h.ID != c.id || h.Name != c.name {
			t.Errorf("AddrHolder(%s, %s) = %+v, %v; want %q (%s)", c.network, c.ip, h, ok, c.id, c.name)
		}
	}
	if h, _, _ := cl.AddrHolder(context.Background(), "ext", "172.24.0.3"); h.Labels[LabelInstance] != "i1" {
		t.Errorf("holder labels = %v; want %s=i1", h.Labels, LabelInstance)
	}
}
