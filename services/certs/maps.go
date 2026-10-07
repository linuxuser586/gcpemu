package certs

import (
	"context"
	"strings"

	cmv1 "google.golang.org/api/certificatemanager/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Certificate maps and entries (FR-LB-004).

// GclbTarget describes a target proxy that serves a certificate map, as
// reported in CertificateMap.gclbTargets.
type GclbTarget struct {
	// TargetHTTPSProxy or TargetSSLProxy is the proxy's relative name
	// ("projects/P/global/targetHttpsProxies/X").
	TargetHTTPSProxy string
	TargetSSLProxy   string
	IPConfigs        []IPConfig
}

// IPConfig is one forwarding rule address of a GclbTarget.
type IPConfig struct {
	IPAddress string
	Ports     []int64
}

// CertificateMapTargets is optionally implemented by the "lb" service: the
// target proxies (and their forwarding rule addresses) that reference a
// certificate map ("projects/P/locations/global/certificateMaps/M"). It
// populates CertificateMap.gclbTargets.
type CertificateMapTargets interface {
	CertificateMapTargets(ctx context.Context, certificateMap string) []GclbTarget
}

// ResourceUsers is optionally implemented by the "lb" service: the
// selfLinks of load balancer resources (target proxies, backend services)
// that reference a Certificate Manager or Network Security resource
// (relative name). Referenced resources cannot be deleted, and
// certificates report them in usedBy.
type ResourceUsers interface {
	CertsResourceUsers(ctx context.Context, name string) []string
}

// prepareEntry validates a certificate map entry.
func (s *Service) prepareEntry(n resName, res, old obj) error {
	e, err := as[cmv1.CertificateMapEntry](res)
	if err != nil {
		return err
	}
	if (e.Hostname == "") == (e.Matcher == "") {
		return apierr.InvalidArgument("Exactly one of hostname or matcher must be set.")
	}
	if e.Matcher != "" && e.Matcher != "PRIMARY" {
		return apierr.InvalidArgument("Invalid value for matcher: %q.", e.Matcher)
	}
	if e.Hostname != "" {
		h := strings.ToLower(strings.TrimSuffix(e.Hostname, "."))
		if !validHostname(strings.TrimPrefix(h, "*.")) {
			return apierr.InvalidArgument("Invalid hostname %q.", e.Hostname)
		}
		res["hostname"] = h
	}
	if len(e.Certificates) == 0 {
		return apierr.InvalidArgument("Field certificates is required.")
	}
	if len(e.Certificates) > 4 {
		return apierr.InvalidArgument("A certificate map entry supports at most 4 certificates.")
	}
	certs := make([]any, len(e.Certificates))
	for i, c := range e.Certificates {
		cn, err := parseName(kCert, c)
		if err != nil {
			return err
		}
		if _, _, err := s.loadKind(kCert, c); err != nil {
			return apierr.InvalidArgument("Certificate %s not found.", cn.name(kCert))
		}
		certs[i] = cn.name(kCert)
	}
	res["certificates"] = certs
	self := n.name(kEntry)
	mapName := n.parent(kEntry)
	var conflict string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, o := range scan(tx, kEntry, resName{Project: n.Project, Location: n.Location, Parent: n.Parent}) {
			if str(o["name"]) == self {
				continue
			}
			if (e.Matcher != "" && str(o["matcher"]) == e.Matcher) || (e.Hostname != "" && str(o["hostname"]) == str(res["hostname"])) {
				conflict = str(o["name"])
			}
		}
		return nil
	})
	if conflict != "" {
		return apierr.AlreadyExists("Certificate map %s already has an entry for this hostname or matcher: %s.", mapName, conflict)
	}
	return nil
}

// certActive reports whether a stored certificate can serve traffic.
func certActive(o obj) bool {
	if o == nil {
		return false
	}
	if _, managed := o["managed"]; managed {
		st, _ := getPath(o, "managed.state")
		return st == "ACTIVE"
	}
	return true
}

// entryState is ACTIVE when every certificate of the entry is ACTIVE.
func entryState(tx store.Tx, o obj) string {
	for _, c := range strs(o["certificates"]) {
		if !certActive(load(tx, c)) {
			return "PENDING"
		}
	}
	return "ACTIVE"
}

// mapTargets returns the gclbTargets of a certificate map.
func (s *Service) mapTargets(ctx context.Context, name string) []any {
	lb, ok := s.env.Lookup("lb")
	if !ok {
		return nil
	}
	p, ok := lb.(CertificateMapTargets)
	if !ok {
		return nil
	}
	var out []any
	for _, t := range p.CertificateMapTargets(ctx, name) {
		g := &cmv1.GclbTarget{TargetHttpsProxy: t.TargetHTTPSProxy, TargetSslProxy: t.TargetSSLProxy}
		for _, ip := range t.IPConfigs {
			g.IpConfigs = append(g.IpConfigs, &cmv1.IpConfig{IpAddress: ip.IPAddress, Ports: ip.Ports})
		}
		out = append(out, mustObj(g))
	}
	return out
}

// lbUsers returns the load balancer resources referencing name.
func (s *Service) lbUsers(ctx context.Context, name string) []string {
	lb, ok := s.env.Lookup("lb")
	if !ok {
		return nil
	}
	if p, ok := lb.(ResourceUsers); ok {
		return p.CertsResourceUsers(ctx, name)
	}
	return nil
}

// selectEntry picks the entry serving sni from a map's entries using
// Certificate Manager's rules: an exact hostname match, then a wildcard
// entry ("*.example.com" matches exactly one extra label), then the
// PRIMARY entry. It returns the candidates in that order of preference.
func selectEntry(entries []obj, sni string) []obj {
	sni = strings.ToLower(strings.TrimSuffix(sni, "."))
	var exact, wild, primary []obj
	for _, e := range entries {
		h := str(e["hostname"])
		switch {
		case str(e["matcher"]) == "PRIMARY":
			primary = append(primary, e)
		case sni == "" || h == "":
		case h == sni:
			exact = append(exact, e)
		case strings.HasPrefix(h, "*."):
			if label, rest, ok := strings.Cut(sni, "."); ok && label != "" && rest == h[2:] {
				wild = append(wild, e)
			}
		}
	}
	return append(append(exact, wild...), primary...)
}
