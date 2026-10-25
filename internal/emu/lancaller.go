package emu

import (
	"context"
	"net"
	"net/http"
	"slices"
)

type lanCallerKey struct{}

// WithLANCaller marks ctx as a request that arrived through a listener on
// the LAN, such as the Google front end's --gfe-bind listeners or a link
// (ADR 0027), at the address ip. Services then report the Data plane
// addresses that the caller's machine can reach, such as Cloud SQL's
// --sql-bind address.
func WithLANCaller(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, lanCallerKey{}, ip)
}

// LANCaller returns the address that WithLANCaller recorded in ctx.
func LANCaller(ctx context.Context) (ip string, ok bool) {
	ip, ok = ctx.Value(lanCallerKey{}).(string)
	return ip, ok
}

// LANServed reports whether the caller in ctx came from another machine
// (a LAN caller), and which of served, the addresses (host or host:port) a
// Data plane is served on on the LAN, to tell it: the one on the address it
// arrived at, else the first, or "" when there is none. It came through the
// LAN when a LAN listener or a link marked it (WithLANCaller), or when it
// reached one of lanIPs, the Instance's LAN addresses such as --bind, that
// isn't loopback. Any other caller is told the addresses it is told
// otherwise.
func LANServed(ctx context.Context, served, lanIPs []string) (addr string, lan bool) {
	ip, ok := LANCaller(ctx)
	if !ok {
		a, _ := ctx.Value(http.LocalAddrContextKey).(*net.TCPAddr)
		if a == nil || a.IP.IsLoopback() {
			return "", false
		}
		ip = a.IP.String()
		if v4 := a.IP.To4(); v4 != nil {
			ip = v4.String()
		}
		if !slices.Contains(lanIPs, ip) {
			return "", false
		}
	}
	for _, a := range served {
		if h, _, err := net.SplitHostPort(a); a == ip || (err == nil && h == ip) {
			return a, true
		}
	}
	if len(served) > 0 {
		return served[0], true
	}
	return "", true
}

type assertedKey struct{}

type asserted struct {
	link string
	p    Principal
}

// WithAsserted marks ctx as a request that a Linked Instance forwarded over
// the link named link, asserting that its caller is p (ADR 0027). The API
// gateway then takes p as the caller, whatever the request's credentials.
func WithAsserted(ctx context.Context, link string, p Principal) context.Context {
	return context.WithValue(ctx, assertedKey{}, asserted{link, p})
}

// AssertedBy returns the link and Principal that WithAsserted recorded in
// ctx.
func AssertedBy(ctx context.Context) (link string, p Principal, ok bool) {
	a, ok := ctx.Value(assertedKey{}).(asserted)
	return a.link, a.p, ok
}
