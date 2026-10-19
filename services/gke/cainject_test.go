package gke

import (
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// parsePod decodes an admitted pod.
func parsePod(t *testing.T, s string) *admitPod {
	t.Helper()
	var p admitPod
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// renderPatch encodes a JSON patch.
func renderPatch(ops []patchOp) string {
	b, _ := json.Marshal(ops)
	return string(b)
}

func TestInjectPatch(t *testing.T) {
	// A bare pod gets the volume, both mounts and every variable.
	ops := injectPatch(parsePod(t, `{"spec":{"containers":[{"name":"a"}],"initContainers":[{"name":"i","env":[{"name":"X"}]}]}}`), "ns")
	got := renderPatch(ops)
	for _, want := range []string{
		`{"op":"add","path":"/spec/volumes","value":[{"configMap":{"items":[{"key":"ca-certificates.crt","path":"ca-certificates.crt"}],"name":"gcpemu-ca-bundle","optional":true},"name":"gcpemu-ca-bundle"}]}`,
		`"path":"/spec/containers/0/volumeMounts","value":[{"mountPath":"/etc/gcpemu/certs","name":"gcpemu-ca-bundle","readOnly":true},{"mountPath":"/etc/ssl/certs/ca-certificates.crt","name":"gcpemu-ca-bundle","readOnly":true,"subPath":"ca-certificates.crt"}]`,
		`"path":"/spec/containers/0/env","value":[{"name":"SSL_CERT_FILE","value":"/etc/gcpemu/certs/ca-certificates.crt"}`,
		`{"name":"GCE_METADATA_HOST","value":"169.254.169.254"}`,
		`{"op":"add","path":"/spec/initContainers/0/env/-","value":{"name":"SSL_CERT_FILE"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("patch lacks %s\n%s", want, got)
		}
	}

	// Existing mounts and variables are respected.
	got = renderPatch(injectPatch(parsePod(t, `{"spec":{"volumes":[{"name":"v"}],"containers":[{"name":"a",
		"volumeMounts":[{"name":"v","mountPath":"/etc/ssl/certs"}],"env":[{"name":"SSL_CERT_FILE"}]}]}}`), "ns"))
	if strings.Contains(got, `"/etc/ssl/certs/ca-certificates.crt"`) || strings.Contains(got, `"SSL_CERT_FILE"`) ||
		!strings.Contains(got, `"path":"/spec/volumes/-"`) || !strings.Contains(got, `"REQUESTS_CA_BUNDLE"`) {
		t.Errorf("patch = %s", got)
	}

	// Opt-out and idempotence.
	if ops := injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/inject":"disabled"}},"spec":{"containers":[{}]}}`), "ns"); ops != nil {
		t.Errorf("opted-out pod patched: %v", ops)
	}
	if ops := injectPatch(parsePod(t, `{"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`), "ns"); ops != nil {
		t.Errorf("injected pod patched again: %v", ops)
	}
}

func TestInjectPatchRealGoogleAPIs(t *testing.T) {
	// A labelled pod resolves through the node's googleapis resolver,
	// keeping the cluster's search path.
	got := renderPatch(injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"containers":[{}]}}`), "ci"))
	for _, want := range []string{
		`{"op":"add","path":"/spec/dnsPolicy","value":"None"}`,
		`{"op":"add","path":"/spec/dnsConfig","value":{"nameservers":["169.254.169.253"],"options":[{"name":"ndots","value":"5"}],"searches":["ci.svc.cluster.local","svc.cluster.local","cluster.local"]}}`,
		`"SSL_CERT_FILE"`, // still trusts the CA, for LOCATION-docker.pkg.dev
	} {
		if !strings.Contains(got, want) {
			t.Errorf("patch lacks %s\n%s", want, got)
		}
	}

	// Already injected (reinvocation): only the DNS settings are added.
	got = renderPatch(injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`), "ci"))
	if !strings.Contains(got, `"/spec/dnsPolicy"`) || strings.Contains(got, `"/spec/volumes`) {
		t.Errorf("patch = %s", got)
	}

	// A pod with its own DNS (or patched already), or without the label,
	// is left alone.
	for _, pod := range []string{
		`{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"dnsPolicy":"None","volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`,
		`{"metadata":{"labels":{"gcpemu.dev/googleapis":"emulated"}},"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`,
	} {
		if ops := injectPatch(parsePod(t, pod), "ci"); ops != nil {
			t.Errorf("%s patched: %s", pod, renderPatch(ops))
		}
	}

	// A dnsConfig of the pod's own is kept after the resolver's settings.
	got = renderPatch(injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],
		"dnsConfig":{"nameservers":["10.0.0.53"],"searches":["corp.internal"],"options":[{"name":"ndots","value":"2"},{"name":"edns0"}]},"containers":[{}]}}`), "ci"))
	if want := `{"op":"add","path":"/spec/dnsConfig","value":{"nameservers":["169.254.169.253","10.0.0.53"],"options":[{"name":"ndots","value":"2"},{"name":"edns0"}],"searches":["ci.svc.cluster.local","svc.cluster.local","cluster.local","corp.internal"]}}`; !strings.Contains(got, want) {
		t.Errorf("patch lacks %s\n%s", want, got)
	}
}

func TestNodeDNSAnswer(t *testing.T) {
	ip := net.ParseIP(metadataIP)
	ans := nodeDNSAnswer(nodeConfig{Frontend: "10.0.0.1:443", FrontendHosts: []string{"storage.googleapis.com"}}, ip)
	for name, want := range map[string]bool{
		"metadata.google.internal": true, "storage.googleapis.com": true, "us-docker.pkg.dev": true,
		"logging.googleapis.com": false, "example.com": false,
	} {
		if got := ans(name) != nil; got != want {
			t.Errorf("%s answered = %v, want %v", name, got, want)
		}
	}
	// Without a frontend only the metadata names are answered.
	ans = nodeDNSAnswer(nodeConfig{FrontendHosts: []string{"storage.googleapis.com"}}, ip)
	if ans("storage.googleapis.com") != nil || ans("metadata.google.internal") == nil {
		t.Error("frontend names answered without a frontend")
	}
}

func TestClusterDNS(t *testing.T) {
	// k3s gives kube-dns the 10th address of the service range.
	if got, err := clusterDNS("34.118.224.0/20"); err != nil || got != "34.118.224.10" {
		t.Errorf("clusterDNS = %q, %v", got, err)
	}
	if _, err := clusterDNS(""); err == nil {
		t.Error("clusterDNS accepted no range")
	}
}

func TestGoogleAPIsDNS(t *testing.T) {
	// Each upstream answers every A query with its own address and
	// records the VPC network the query carried.
	upstream := func(ip string) (string, *atomic.Value) {
		t.Helper()
		l, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var network atomic.Value
		srv := &mdns.Server{PacketConn: l, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, r *mdns.Msg) {
			network.Store(frontend.Network(r))
			m := new(mdns.Msg)
			m.SetReply(r)
			m.Answer = append(m.Answer, &mdns.A{Hdr: mdns.RR_Header{Name: r.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 5}, A: net.ParseIP(ip)})
			_ = w.WriteMsg(m)
		})}
		go func() { _ = srv.ActivateAndServe() }()
		t.Cleanup(func() { _ = srv.Shutdown() })
		return l.LocalAddr().String(), &network
	}
	cloud, cloudNet := upstream("192.0.2.1")
	cluster, clusterNet := upstream("192.0.2.2")

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &mdns.Server{PacketConn: pc, Handler: googleAPIsDNS(cloud, cluster, "projects/p/global/networks/n")}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	ask := func(name string) string {
		t.Helper()
		m := new(mdns.Msg)
		m.SetQuestion(name, mdns.TypeA)
		c := &mdns.Client{}
		for i := 0; i < 20; i++ { // server start race
			if r, _, err := c.Exchange(m, pc.LocalAddr().String()); err == nil && len(r.Answer) == 1 {
				return r.Answer[0].(*mdns.A).A.String()
			}
		}
		t.Fatalf("query %s failed", name)
		return ""
	}
	// Google APIs resolve through Cloud DNS, on the node's network, as on GCP.
	for _, name := range []string{"storage.googleapis.com.", "GoogleAPIs.com."} {
		if got := ask(name); got != "192.0.2.1" {
			t.Errorf("%s = %s, want Cloud DNS's answer", name, got)
		}
	}
	if got := cloudNet.Load(); got != "projects/p/global/networks/n" {
		t.Errorf("Cloud DNS query network = %v", got)
	}
	// Everything else, Artifact Registry and search-path expansions of
	// Google APIs included, resolves through the cluster's DNS.
	for _, name := range []string{"kubernetes.default.svc.cluster.local.", "us-docker.pkg.dev.",
		"storage.googleapis.com.ci.svc.cluster.local.", "example.com."} {
		if got := ask(name); got != "192.0.2.2" {
			t.Errorf("%s = %s, want the cluster DNS's answer", name, got)
		}
	}
	if got := clusterNet.Load(); got != "" {
		t.Errorf("cluster DNS query network = %v", got)
	}
}
