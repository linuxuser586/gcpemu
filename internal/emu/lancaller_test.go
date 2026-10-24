package emu

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// arrivedAt is a request context whose connection's local address is ip.
func arrivedAt(ip string) context.Context {
	return context.WithValue(context.Background(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(ip), Port: 443})
}

// A caller arrived through the LAN when a LAN listener or a link marked
// it, or when it reached a LAN address that isn't loopback (--bind or a
// --*-bind address). It is then told the LAN address it can reach: the one
// it came in on, or else the first served, or none when the Data plane
// isn't served there.
func TestLANServed(t *testing.T) {
	served := []string{"192.168.1.5", "127.0.0.1"}
	lanIPs := []string{"192.168.1.5", "127.0.0.1", "192.168.1.7"}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		served []string
		want   string
	}{
		{"loopback", arrivedAt("127.0.0.1"), served, ""},
		{"loopback, no address served", arrivedAt("127.0.0.1"), nil, ""},
		{"a --sql-bind address", arrivedAt("192.168.1.5"), served, "192.168.1.5"},
		{"a container network", arrivedAt("172.18.0.1"), served, ""},
		{"--bind, not a --sql-bind address", arrivedAt("192.168.1.7"), served, "192.168.1.5"},
		{"--gfe-bind", WithLANCaller(arrivedAt("192.168.1.9"), "192.168.1.9"), served, "192.168.1.5"},
		{"--gfe-bind on a --sql-bind address", WithLANCaller(arrivedAt("127.0.0.1"), "127.0.0.1"), served, "127.0.0.1"},
		{"a link, served on a port", WithLANCaller(context.Background(), "192.168.1.7"), []string{"192.168.1.5:41000", "192.168.1.7:41000"}, "192.168.1.7:41000"},
	} {
		got, ok := LANServed(tc.ctx, tc.served, lanIPs)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: LANServed = %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}
	for name, ctx := range map[string]context.Context{"--gfe-bind": WithLANCaller(context.Background(), "192.168.1.9"), "--bind": arrivedAt("192.168.1.7")} {
		if ip, ok := LANServed(ctx, nil, lanIPs); !ok || ip != "" {
			t.Errorf("%s caller of an instance not served on the LAN: %q, %v; want a LAN caller with no address", name, ip, ok)
		}
	}
}
