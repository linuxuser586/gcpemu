package compute

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestProxyAliasIsolated (#173): the NAT egress proxy aliases each Translated
// network on its own, so one whose VLAN link the kernel can't create
// fails with a clear error while the others, and the NAT rules, are
// unaffected.
func TestProxyAliasIsolated(t *testing.T) {
	var mu sync.Mutex
	execs := map[string]string{} // exec id -> script
	var scripts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/containers/gw/exec"):
			var body struct{ Cmd []string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			id := "e" + string(rune('0'+len(execs)))
			execs[id] = body.Cmd[2]
			scripts = append(scripts, body.Cmd[2])
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case strings.HasSuffix(r.URL.Path, "/start"):
			if strings.Contains(execs[execID(r.URL.Path)], "a=10.241.") {
				msg := "Error: Unknown device type.\ngcpemu: creating the VLAN link failed\n"
				hdr := make([]byte, 8)
				hdr[0] = 2
				binary.BigEndian.PutUint32(hdr[4:], uint32(len(msg)))
				_, _ = w.Write(append(hdr, msg...))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			code := 0
			if strings.Contains(execs[execID(r.URL.Path)], "a=10.241.") {
				code = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"ExitCode": code})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &runtime.Manager{Client: cl, Info: runtime.Info{Name: "docker", Version: "27.3.1", OS: "Colima"}}
	s, _ := natTestService(t, nil)
	nets := []*vpcNet{
		{Name: "gcpemu-a", Range: "10.0.0.0/24", Subnet: "10.241.0.0/24", Fallback: true, Translated: true, Network: tNet},
		{Name: "gcpemu-b", Range: "10.0.1.0/24", Subnet: "10.242.0.0/24", Fallback: true, Translated: true, Network: tNet},
		{Name: "gcpemu-c", Range: "10.0.2.0/24", Subnet: "10.0.2.0/24", Network: tNet},
	}
	g := &egressProxy{s: s, network: tNet, id: "gw", extIP: "172.22.0.2", extGW: "172.22.0.1"}

	g.alias(context.Background(), rt, nets)
	var nv *emu.NoVLANError
	if err := g.aliasError("gcpemu-a"); !errors.As(err, &nv) || !strings.Contains(err.Error(), "docker 27.3.1 (Colima)") {
		t.Fatalf("gcpemu-a: %v; want NoVLANError naming the runtime", err)
	}
	for _, n := range []string{"gcpemu-b", "gcpemu-c"} {
		if err := g.aliasError(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if len(scripts) != 2 {
		t.Fatalf("aliased %d networks; want the 2 Translated ones", len(scripts))
	}
	// A later sync retries only the network that failed.
	g.alias(context.Background(), rt, nets)
	if len(scripts) != 3 || !strings.Contains(scripts[2], "a=10.241.") {
		t.Fatalf("second sync ran %d scripts", len(scripts)-2)
	}
	// The NAT rules don't alias, so a failed alias can't abort them.
	rules, err := s.natRules(g, nets)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rules, "type vlan") {
		t.Fatalf("NAT rules alias:\n%s", rules)
	}
}

// execID is the exec id in /exec/ID/start or /exec/ID/json.
func execID(path string) string {
	p := strings.Split(path, "/")
	return p[len(p)-2]
}
