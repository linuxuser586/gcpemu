package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
