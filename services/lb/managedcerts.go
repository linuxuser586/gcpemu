package lb

import (
	"context"
	"net"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/certs"
)

// Google-managed SSL certificates (FR-LB-004): a MANAGED certificate is
// PROVISIONING until every domain resolves (through the emulated Cloud
// DNS, or the host resolver for names outside it) to the IP of a
// forwarding rule whose target HTTPS proxy uses the certificate. Then the
// emulator's CA issues it and it becomes ACTIVE; domainStatus reports each
// domain.

func (s *Service) managedCertLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.certKick:
		}
		s.provisionManagedCerts(ctx)
	}
}

// resolver is the subset of the dns service used here.
type resolver interface {
	Resolve(ctx context.Context, name string, qtype uint16) ([]mdns.RR, error)
}

func (s *Service) resolveA(ctx context.Context, name string) []string {
	if svc, ok := s.env.Lookup("dns"); ok {
		if r, ok := svc.(resolver); ok {
			rrs, err := r.Resolve(ctx, name, mdns.TypeA)
			if err == nil {
				var out []string
				for _, rr := range rrs {
					if a, ok := rr.(*mdns.A); ok {
						out = append(out, a.A.String())
					}
				}
				return out
			}
		}
	}
	ips, _ := net.DefaultResolver.LookupHost(ctx, name)
	return ips
}

func (s *Service) provisionManagedCerts(ctx context.Context) {
	s.certMu.Lock()
	defer s.certMu.Unlock()
	changed := false
	for _, obj := range s.loadAll(kindSSLCertificate, "") {
		c := obj.(*computev1.SslCertificate)
		if c.Type != "MANAGED" || c.Managed == nil || c.Managed.Status == "ACTIVE" {
			continue
		}
		path := relPath(c.SelfLink)
		serving := s.servingIPs(path)
		status := map[string]string{}
		all := len(c.Managed.Domains) > 0
		for _, d := range c.Managed.Domains {
			st := "PROVISIONING"
			if len(serving) > 0 {
				rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				for _, ip := range s.resolveA(rctx, d) {
					if serving[ip] {
						st = "ACTIVE"
					}
				}
				cancel()
			}
			if st != "ACTIVE" {
				all = false
			}
			status[d] = st
		}
		next := jsonClone(c)
		next.Managed.DomainStatus = status
		var key string
		if all {
			leaf, err := s.env.CA.Issue(ca.Leaf{DNSNames: c.Managed.Domains, Organization: "gcpemu Google-managed"})
			if err != nil {
				s.env.Log.Warn("lb: issuing managed certificate", "certificate", path, "err", err)
				continue
			}
			chain, keyPEM, err := ca.EncodeCert(leaf)
			if err != nil {
				continue
			}
			next.Certificate = string(chain)
			next.ExpireTime = stamp(leaf.Leaf.NotAfter)
			next.SubjectAlternativeNames = append([]string{}, c.Managed.Domains...)
			next.Managed.Status = "ACTIVE"
			key = string(keyPEM)
		}
		if equalStatus(c.Managed.DomainStatus, status) && !all {
			continue
		}
		_ = s.env.Store.Update(func(tx store.Tx) error {
			if !store.Exists(tx, kindSSLCertificate.ns(), path) {
				return nil
			}
			if key != "" {
				if err := store.PutJSON(tx, nsSSLKeys, path, key); err != nil {
					return err
				}
			}
			return store.PutJSON(tx, kindSSLCertificate.ns(), path, next)
		})
		changed = changed || all
		if all {
			s.env.Log.Info("lb: managed certificate active", "certificate", path, "domains", strings.Join(c.Managed.Domains, ","))
		}
	}
	if changed {
		s.dp.reconcile()
	}
}

func equalStatus(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// servingIPs returns the IPs (forwarding rule IPs and their mapped
// listener IPs) of forwarding rules whose HTTPS proxy uses certificate.
func (s *Service) servingIPs(certificate string) map[string]bool {
	out := map[string]bool{}
	cfg := s.dp.cfg.Load()
	for _, fe := range cfg.fronts {
		if !fe.https {
			continue
		}
		obj, ok := s.load(kindTargetHTTPSProxy, fe.proxyPath)
		if !ok {
			continue
		}
		for _, c := range obj.(*computev1.TargetHttpsProxy).SslCertificates {
			if relPath(c) == certificate {
				out[fe.ip] = true
				if m := s.dp.mappedIP(net.ParseIP(fe.ip)); m != nil {
					out[m.String()] = true
				}
			}
		}
	}
	return out
}

// --- hooks for the certs service ---

// ForwardingRuleIPs returns the forwarding rule IPs of a project and their
// mapped listener IPs (Certificate Manager load balancer authorization).
func (s *Service) ForwardingRuleIPs(ctx context.Context, project string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(ip string) {
		if ip != "" && !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	for _, obj := range s.loadAll(kindForwardingRule, "projects/"+project+"/") {
		f := obj.(*computev1.ForwardingRule)
		add(f.IPAddress)
		if m := s.dp.mappedIP(net.ParseIP(f.IPAddress)); m != nil {
			add(m.String())
		}
	}
	return out
}

// CertificateMapTargets lists the target HTTPS proxies using a certificate
// map with their forwarding rule addresses (Certificate Manager gclbTargets).
func (s *Service) CertificateMapTargets(ctx context.Context, certificateMap string) []certs.GclbTarget {
	want := certsRel(certificateMap)
	var out []certs.GclbTarget
	for _, obj := range s.loadAll(kindTargetHTTPSProxy, "") {
		p := obj.(*computev1.TargetHttpsProxy)
		if p.CertificateMap == "" || certsRel(p.CertificateMap) != want {
			continue
		}
		t := certs.GclbTarget{TargetHTTPSProxy: relPath(p.SelfLink)}
		byIP := map[string][]int64{}
		var order []string
		for _, fo := range s.loadAll(kindForwardingRule, "") {
			f := fo.(*computev1.ForwardingRule)
			if relPath(f.Target) != t.TargetHTTPSProxy {
				continue
			}
			lo, _, _ := strings.Cut(f.PortRange, "-")
			if _, ok := byIP[f.IPAddress]; !ok {
				order = append(order, f.IPAddress)
			}
			byIP[f.IPAddress] = append(byIP[f.IPAddress], int64(atoiSafe(lo)))
		}
		for _, ip := range order {
			t.IPConfigs = append(t.IPConfigs, certs.IPConfig{IPAddress: ip, Ports: byIP[ip]})
		}
		out = append(out, t)
	}
	return out
}

// CertsResourceUsers returns load balancer resources referencing a
// Certificate Manager or Network Security resource (certificate maps,
// server TLS policies, backend authentication configs).
func (s *Service) CertsResourceUsers(ctx context.Context, name string) []string {
	want := certsRel(name)
	var out []string
	for _, obj := range s.loadAll(kindTargetHTTPSProxy, "") {
		p := obj.(*computev1.TargetHttpsProxy)
		if (p.CertificateMap != "" && certsRel(p.CertificateMap) == want) ||
			(p.ServerTlsPolicy != "" && certsRel(p.ServerTlsPolicy) == want) {
			out = append(out, p.SelfLink)
		}
	}
	for _, obj := range s.loadAll(kindBackendService, "") {
		b := obj.(*computev1.BackendService)
		if b.TlsSettings != nil && b.TlsSettings.AuthenticationConfig != "" && certsRel(b.TlsSettings.AuthenticationConfig) == want {
			out = append(out, b.SelfLink)
		}
	}
	return out
}

// certsRel reduces a Certificate Manager / Network Security reference to
// its relative name "projects/P/locations/L/...".
func certsRel(s string) string {
	s = strings.TrimPrefix(s, certManagerPrefix)
	s = strings.TrimPrefix(s, netSecPrefix)
	return relPath(s)
}
