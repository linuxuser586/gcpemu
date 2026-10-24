package emu

import "context"

type lanCallerKey struct{}

// WithLANCaller marks ctx as a request that arrived through a listener on
// the LAN, such as the Google front end's --gfe-bind listeners, at the
// address ip. Services then report the Data plane addresses that the
// caller's machine can reach, such as Cloud SQL's --sql-bind address.
func WithLANCaller(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, lanCallerKey{}, ip)
}

// LANCaller returns the address that WithLANCaller recorded in ctx.
func LANCaller(ctx context.Context) (ip string, ok bool) {
	ip, ok = ctx.Value(lanCallerKey{}).(string)
	return ip, ok
}
