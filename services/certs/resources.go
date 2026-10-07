package certs

import (
	"context"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Per-kind hooks of the resource engine.

// prepare validates and completes res (old is nil on create). The returned
// commit, if any, runs inside the write transaction.
func (s *Service) prepare(ctx context.Context, k *kind, n resName, res, old obj) (func(store.Tx) error, error) {
	switch k {
	case kCert:
		return s.prepareCert(ctx, n, res, old)
	case kEntry:
		return nil, s.prepareEntry(n, res, old)
	case kDNSAuth:
		return nil, s.prepareDNSAuth(n, res, old)
	case kTrust:
		return nil, prepareTrust(res)
	case kIssuance:
		return nil, prepareIssuance(res)
	case kBAC:
		return nil, s.prepareBAC(n, res)
	case kServerTLS:
		return nil, s.prepareServerTLS(res)
	}
	return nil, nil
}

// view returns a stored resource with its computed output fields.
func (s *Service) view(ctx context.Context, k *kind, o obj) obj {
	v := clone(o)
	name := str(o["name"])
	switch k {
	case kCert:
		var used []any
		_ = s.env.Store.View(func(tx store.Tx) error {
			for _, e := range scanAll(tx, kEntry) {
				for _, c := range strs(e["certificates"]) {
					if c == name {
						used = append(used, map[string]any{"name": str(e["name"])})
					}
				}
			}
			return nil
		})
		for _, u := range s.lbUsers(ctx, name) {
			used = append(used, map[string]any{"name": u})
		}
		if len(used) > 0 {
			v["usedBy"] = used
		}
	case kMap:
		if t := s.mapTargets(ctx, name); len(t) > 0 {
			v["gclbTargets"] = t
		}
	case kEntry:
		_ = s.env.Store.View(func(tx store.Tx) error { v["state"] = entryState(tx, o); return nil })
	}
	return v
}

// inUse returns FAILED_PRECONDITION when name is still referenced.
func (s *Service) inUse(ctx context.Context, k *kind, name string) error {
	var users []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		switch k {
		case kCert:
			for _, e := range scanAll(tx, kEntry) {
				for _, c := range strs(e["certificates"]) {
					if c == name {
						users = append(users, str(e["name"]))
					}
				}
			}
			for _, b := range scanAll(tx, kBAC) {
				if refersTo(str(b["clientCertificate"]), name) {
					users = append(users, str(b["name"]))
				}
			}
		case kMap:
			n, _ := parseName(kMap, name)
			for _, e := range scan(tx, kEntry, resName{Project: n.Project, Location: n.Location, Parent: n.ID}) {
				users = append(users, str(e["name"]))
			}
		case kDNSAuth:
			for _, c := range scanAll(tx, kCert) {
				v, _ := getPath(c, "managed.dnsAuthorizations")
				for _, a := range strs(v) {
					if refersTo(a, name) {
						users = append(users, str(c["name"]))
					}
				}
			}
		case kIssuance:
			for _, c := range scanAll(tx, kCert) {
				if v, _ := getPath(c, "managed.issuanceConfig"); refersTo(str(v), name) {
					users = append(users, str(c["name"]))
				}
			}
		case kTrust:
			for _, b := range scanAll(tx, kBAC) {
				if refersTo(str(b["trustConfig"]), name) {
					users = append(users, str(b["name"]))
				}
			}
			for _, p := range scanAll(tx, kServerTLS) {
				if v, _ := getPath(p, "mtlsPolicy.clientValidationTrustConfig"); refersTo(str(v), name) {
					users = append(users, str(p["name"]))
				}
			}
		}
		return nil
	})
	if k != kEntry && k != kDNSAuth && k != kIssuance && k != kTrust {
		users = append(users, s.lbUsers(ctx, name)...)
	}
	if len(users) > 0 {
		return apierr.FailedPrecondition("Resource '%s' is in use by: %s", name, strings.Join(users, ", ")).
			WithReason(k.host(), "RESOURCE_IN_USE")
	}
	return nil
}

// afterWrite runs after a committed mutation.
func (s *Service) afterWrite(k *kind, name string) {
	s.invalidate()
	if k == kCert || k == kDNSAuth {
		s.kick()
	}
}
