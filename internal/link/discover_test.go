package link_test

import (
	"net"
	"os"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
	"github.com/linuxuser586/gcpemu/internal/link"
	"github.com/linuxuser586/gcpemu/internal/mdns"
)

// onLAN serves a linked Instance's link protocol on ip instead of
// loopback, as --link-bind on a LAN address does, and advertises it when
// advertise is set.
func onLAN(t *testing.T, name, ip string, port int, advertise bool, opts ...func(*link.Config)) *linked {
	t.Helper()
	opts = append(opts, func(c *link.Config) { c.MDNS = mdns.Options{Port: port} })
	l := newLinkedOn(t, name, "", ip, opts...)
	if advertise {
		if err := l.Advertise([]string{l.addr}); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// discovered returns what l discovers, by Instance name.
func discovered(t *testing.T, l *linked) map[string]link.Discovered {
	t.Helper()
	ds, err := l.Discover(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]link.Discovered{}
	for _, d := range ds {
		out[d.Instance] = d
	}
	return out
}

// hostLabel is the host part of a discovered name on this machine.
func hostLabel(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// Instances with --link-bind on the same LAN each list the other, named
// "<Instance name>@<host>", with the address, Instance ID and CA
// fingerprint they advertise; an Instance that does not advertise is not
// listed, nor is the Instance itself. Pairing marks the other Instance
// with its link.
func TestDiscoverOverMDNS(t *testing.T) {
	ip, port := hosttest.LANIP(t), hosttest.FreeUDPPort(t)
	a := onLAN(t, "laptop", ip, port, true)
	b := onLAN(t, "office", ip, port, true)
	onLAN(t, "quiet", ip, port, false)

	got := discovered(t, a)
	if _, ok := got["laptop"]; ok {
		t.Fatal("an Instance lists itself")
	}
	if _, ok := got["quiet"]; ok {
		t.Fatal("an Instance that does not advertise is listed")
	}
	d, ok := got["office"]
	if !ok {
		t.Fatalf("office not discovered: %+v", got)
	}
	want := link.Discovered{
		Name: "office@" + hostLabel(t), Instance: "office", InstanceID: "office-id", Address: b.addr,
		Fingerprint: link.Fingerprint(b.ca.Certificate()), Version: 1, MinVersion: 1, Source: link.SourceMDNS,
	}
	if d != want {
		t.Fatalf("discovered\n %+v\nwant\n %+v", d, want)
	}
	if _, ok := discovered(t, b)["laptop"]; !ok {
		t.Fatal("office does not discover laptop")
	}

	pair(t, a, b)
	if d := discovered(t, a)["office"]; d.Link != "office" {
		t.Fatalf("paired Instance not marked: %+v", d)
	}
}

// A Discovered Instance speaking an incompatible link protocol version is
// listed with a problem naming both versions.
func TestDiscoverMarksIncompatible(t *testing.T) {
	ip, port := hosttest.LANIP(t), hosttest.FreeUDPPort(t)
	a := onLAN(t, "laptop", ip, port, true)
	onLAN(t, "future", ip, port, true, func(c *link.Config) { c.Versions = link.Versions{Min: 7, Max: 8} })

	d := discovered(t, a)["future"]
	if !strings.Contains(d.Problem, "version 1") || !strings.Contains(d.Problem, "version 7-8") {
		t.Fatalf("incompatible Instance's problem %q", d.Problem)
	}
}

// Static link addresses, for networks multicast doesn't cross, are asked
// who they are and listed as found there; one that cannot be reached is
// listed with the problem.
func TestDiscoverStaticAddresses(t *testing.T) {
	port := hosttest.FreeUDPPort(t)
	b := newLinked(t, "ci", "")
	a := newLinked(t, "runner", "", func(c *link.Config) {
		c.MDNS = mdns.Options{Port: port}
		c.StaticAddrs = []string{b.addr, "127.0.0.1:1"}
	})
	ds, err := a.Discover(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var found, unreachable bool
	for _, d := range ds {
		switch d.Address {
		case b.addr:
			host, _, _ := net.SplitHostPort(b.addr)
			found = d.Name == "ci@"+host && d.InstanceID == "ci-id" && d.Source == link.SourceStatic &&
				d.Fingerprint == link.Fingerprint(b.ca.Certificate()) && d.Problem == ""
		case "127.0.0.1:1":
			unreachable = d.Source == link.SourceStatic && d.Problem != ""
		}
	}
	if !found || !unreachable {
		t.Fatalf("static addresses listed as %+v", ds)
	}
}
