package gke_test

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// testHostServices is ADR 0014's outward Host attachment: host clients
// resolve c1's ClusterIP Service web-host (the NEG test's pods) by its
// short and qualified names to a Loopback alias, and reach a pod there.
// The short name stops resolving while the private cluster has a Service
// of the same name, and the alias is released with the Service. Queries
// carrying a VPC network are not answered with it.
func testHostServices(t *testing.T, e, priv *env) {
	if !loopback.Aliases() {
		t.Skip("needs loopback aliases (sudo gcpemu loopback install)")
	}
	dnsAddr := e.inst.Endpoint("dns")
	lookup := func(name, network string) (string, int) {
		m := new(mdns.Msg)
		m.SetQuestion(name, mdns.TypeA)
		if network != "" {
			m = frontend.SetNetwork(m, network)
		}
		r, _, err := (&mdns.Client{Timeout: 5 * time.Second}).Exchange(m, dnsAddr)
		if err != nil {
			t.Logf("%s: %v", name, err)
			return "", -1
		}
		for _, rr := range r.Answer {
			if a, ok := rr.(*mdns.A); ok {
				return a.A.String(), r.Rcode
			}
		}
		return "", r.Rcode
	}
	hostService := map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "web-host"},
		"spec": map[string]any{"selector": map[string]string{"app": "web"}, "ports": []map[string]any{{"port": 18080, "targetPort": 8080}}},
	}
	e.mustKube("POST", "/api/v1/namespaces/default/services", hostService, nil)
	const short = "web-host.default.svc.cluster.local."
	qualified := "web-host.default.svc.c1." + zone + "." + project + ".gke.internal."
	var ip string
	eventually(t, 30*time.Second, short+" resolving", func() bool {
		var rcode int
		ip, rcode = lookup(short, "")
		return rcode == mdns.RcodeSuccess && ip != ""
	})
	if !net.ParseIP(ip).IsLoopback() || ip == loopback.Host {
		t.Fatalf("%s = %s, want a Loopback alias", short, ip)
	}
	if got, _ := lookup(qualified, ""); got != ip {
		t.Errorf("%s = %s, want %s", qualified, got, ip)
	}
	if got, _ := lookup(short, e.c.GetNetworkConfig().GetNetwork()); got == ip {
		t.Errorf("a query carrying the VPC network got the alias")
	}
	addr := net.JoinHostPort(ip, "18080")
	eventually(t, 30*time.Second, "web-host reachable from the host at "+addr, func() bool {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(b)) == "ok"
	})

	// A second cluster with the Service takes the short name away until it
	// deletes it; the qualified names still resolve.
	priv.mustKube("POST", "/api/v1/namespaces/default/services", map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "web-host"},
		"spec": map[string]any{"ports": []map[string]any{{"port": 18080}}},
	}, nil)
	eventually(t, 10*time.Second, short+" not resolving with two clusters", func() bool {
		_, rcode := lookup(short, "")
		return rcode == mdns.RcodeNameError
	})
	if got, _ := lookup(qualified, ""); got != ip {
		t.Errorf("%s with two clusters = %s, want %s", qualified, got, ip)
	}
	if got, _ := lookup("web-host.default.svc.priv."+zone+"."+project+".gke.internal.", ""); got == "" || got == ip {
		t.Errorf("the private cluster's Service at %q", got)
	}
	var report struct {
		Collisions []struct {
			Name     string
			Clusters []string
		}
	}
	// Every cluster's built-in Services (kubernetes.default, kube-dns)
	// collide too.
	eventually(t, 30*time.Second, "the collision listed", func() bool {
		gw(t, e.inst, "GET", "/_emu/v1/host/services", nil, &report)
		for _, c := range report.Collisions {
			if c.Name == strings.TrimSuffix(short, ".") {
				return len(c.Clusters) == 2
			}
		}
		return false
	})
	priv.mustKube("DELETE", "/api/v1/namespaces/default/services/web-host", nil, nil)
	eventually(t, 10*time.Second, short+" resolving again", func() bool {
		got, _ := lookup(short, "")
		return got == ip
	})

	// Deleting the Service releases its alias.
	e.mustKube("DELETE", "/api/v1/namespaces/default/services/web-host", nil, nil)
	var held struct{ Services []struct{ Address string } }
	eventually(t, 30*time.Second, "the alias released", func() bool {
		gw(t, e.inst, "GET", "/_emu/v1/host/services", nil, &held)
		for _, s := range held.Services {
			if s.Address == ip {
				return false
			}
		}
		return true
	})
	if _, rcode := lookup(short, ""); rcode != mdns.RcodeNameError {
		t.Errorf("%s after the Service went: %s", short, mdns.RcodeToString[rcode])
	}
}
