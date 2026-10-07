package certs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
	cmv1 "google.golang.org/api/certificatemanager/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Certificates (FR-LB-004): self-managed certificates are validated and
// stored with their private key; managed certificates stay PROVISIONING
// until every domain is authorized — by the CNAME of a DNS authorization
// in the emulated Cloud DNS, or (load balancer authorization) by the
// domain resolving to a forwarding rule IP — and are then issued by the
// instance CA and become ACTIVE. Private PKI certificates (issuanceConfig)
// are issued by the instance CA without domain authorization.

// keyRecord is the private part of a certificate (store namespace nsKeys).
type keyRecord struct {
	Chain string `json:"chain"`
	Key   string `json:"key"`
	// Issuer is the SHA-256 of the CA that issued a managed certificate,
	// to re-issue after `gcpemu ca rotate`.
	Issuer string `json:"issuer,omitempty"`
}

var certScopes = map[string]bool{"DEFAULT": true, "EDGE_CACHE": true, "ALL_REGIONS": true, "CLIENT_AUTH": true}

// prepareCert validates a certificate on create (old == nil) or update.
func (s *Service) prepareCert(ctx context.Context, n resName, res, old obj) (func(store.Tx) error, error) {
	c, err := as[cmv1.Certificate](res)
	if err != nil {
		return nil, err
	}
	name := n.name(kCert)
	if c.Scope != "" && !certScopes[c.Scope] {
		return nil, apierr.InvalidArgument("Invalid value for scope: %q.", c.Scope)
	}
	if (c.Scope == "EDGE_CACHE" || c.Scope == "ALL_REGIONS") && n.Location != "global" {
		return nil, apierr.InvalidArgument("Certificates with scope %s must be created in location \"global\".", c.Scope)
	}
	if old == nil {
		if (c.SelfManaged == nil) == (c.Managed == nil) {
			return nil, apierr.InvalidArgument("Exactly one of self_managed or managed must be set.")
		}
	} else {
		if _, wasManaged := old["managed"]; wasManaged && c.SelfManaged != nil {
			return nil, apierr.InvalidArgument("self_managed cannot be set on a managed certificate.")
		}
		// Output-only managed state survives updates.
		for _, p := range []string{"managed.state", "managed.provisioningIssue", "managed.authorizationAttemptInfo"} {
			v, ok := getPath(old, p)
			setPath(res, p, v, ok)
		}
	}
	if c.SelfManaged != nil {
		chain, key, leaf, err := parseKeyPair(c.SelfManaged.PemCertificate, c.SelfManaged.PemPrivateKey)
		if err != nil {
			return nil, err
		}
		res["pemCertificate"] = chain
		res["expireTime"] = leaf.NotAfter.UTC().Format(time.RFC3339)
		if sans := leaf.DNSNames; len(sans) > 0 {
			res["sanDnsnames"] = toAny(sans)
		} else {
			delete(res, "sanDnsnames")
		}
		delete(res, "selfManaged")
		rec := keyRecord{Chain: chain, Key: key}
		return func(tx store.Tx) error { return store.PutJSON(tx, nsKeys, name, rec) }, nil
	}
	if old != nil || c.Managed == nil {
		return nil, nil
	}
	m := c.Managed
	if len(m.Domains) == 0 {
		return nil, apierr.InvalidArgument("Field managed.domains is required.")
	}
	if len(m.Domains) > 100 {
		return nil, apierr.InvalidArgument("A managed certificate supports at most 100 domains.")
	}
	auths := map[string]bool{}
	for i, a := range m.DnsAuthorizations {
		an, err := parseName(kDNSAuth, a)
		if err != nil {
			return nil, err
		}
		if an.Project != n.Project {
			return nil, apierr.InvalidArgument("DNS authorization %s must be in project %s.", a, n.Project)
		}
		o, _, err := s.loadKind(kDNSAuth, a)
		if err != nil {
			return nil, apierr.InvalidArgument("DNS authorization %s not found.", a)
		}
		m.DnsAuthorizations[i] = an.name(kDNSAuth)
		auths[strings.ToLower(str(o["domain"]))] = true
	}
	if m.IssuanceConfig != "" {
		in, err := parseName(kIssuance, m.IssuanceConfig)
		if err != nil {
			return nil, err
		}
		if _, _, err := s.loadKind(kIssuance, m.IssuanceConfig); err != nil {
			return nil, apierr.InvalidArgument("Certificate issuance config %s not found.", m.IssuanceConfig)
		}
		m.IssuanceConfig = in.name(kIssuance)
		if len(m.DnsAuthorizations) > 0 {
			return nil, apierr.InvalidArgument("dns_authorizations cannot be combined with issuance_config.")
		}
	}
	for i, d := range m.Domains {
		d = strings.ToLower(strings.TrimSuffix(d, "."))
		m.Domains[i] = d
		base, wild := strings.CutPrefix(d, "*.")
		if !validHostname(base) {
			return nil, apierr.InvalidArgument("Invalid domain name %q.", d)
		}
		if m.IssuanceConfig != "" {
			continue
		}
		if wild && len(m.DnsAuthorizations) == 0 {
			return nil, apierr.InvalidArgument("Wildcard domain %q requires DNS authorization.", d)
		}
		if len(m.DnsAuthorizations) > 0 && !auths[base] {
			return nil, apierr.InvalidArgument("Domain %q is not covered by any of the provided DNS authorizations.", d)
		}
	}
	m.State = "PROVISIONING"
	m.ProvisioningIssue = nil
	m.AuthorizationAttemptInfo = nil
	for _, d := range m.Domains {
		m.AuthorizationAttemptInfo = append(m.AuthorizationAttemptInfo, &cmv1.AuthorizationAttemptInfo{Domain: d, State: "AUTHORIZING"})
	}
	res["managed"] = mustObj(m)
	res["sanDnsnames"] = toAny(m.Domains)
	delete(res, "pemCertificate")
	delete(res, "expireTime")
	return nil, nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// validHostname reports whether h is a DNS name of letters, digits and
// hyphens with at least two labels.
func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

// parseKeyPair validates a PEM chain and private key and returns them
// normalised (chain re-encoded, key as given) with the parsed leaf.
func parseKeyPair(chainPEM, keyPEM string) (string, string, *x509.Certificate, error) {
	if strings.TrimSpace(chainPEM) == "" {
		return "", "", nil, apierr.InvalidArgument("Field self_managed.pem_certificate is required.")
	}
	if strings.TrimSpace(keyPEM) == "" {
		return "", "", nil, apierr.InvalidArgument("Field self_managed.pem_private_key is required.")
	}
	var certs []*x509.Certificate
	var chain strings.Builder
	rest := []byte(chainPEM)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return "", "", nil, apierr.InvalidArgument("Failed to parse the PEM certificate: %v", err)
		}
		certs = append(certs, c)
		chain.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes}))
	}
	if len(certs) == 0 {
		return "", "", nil, apierr.InvalidArgument("Failed to parse the PEM certificate: no CERTIFICATE block found.")
	}
	if _, err := tls.X509KeyPair([]byte(chain.String()), []byte(keyPEM)); err != nil {
		return "", "", nil, apierr.InvalidArgument("The private key does not match the certificate or cannot be parsed: %v", err)
	}
	return chain.String(), keyPEM, certs[0], nil
}

// ---- DNS authorizations ----

// prepareDNSAuth validates a DNS authorization and computes its record.
func (s *Service) prepareDNSAuth(n resName, res, old obj) error {
	if old != nil {
		res["dnsResourceRecord"] = old["dnsResourceRecord"]
		return nil
	}
	d := strings.ToLower(strings.TrimSuffix(str(res["domain"]), "."))
	if d == "" {
		return apierr.InvalidArgument("Field domain is required.")
	}
	if strings.HasPrefix(d, "*.") || !validHostname(d) {
		return apierr.InvalidArgument("Invalid domain %q: must be a fully qualified domain name without a wildcard.", d)
	}
	res["domain"] = d
	t := str(res["type"])
	switch t {
	case "":
		t = "FIXED_RECORD"
		if n.Location != "global" {
			t = "PER_PROJECT_RECORD"
		}
	case "FIXED_RECORD":
		if n.Location != "global" {
			return apierr.InvalidArgument("DNS authorizations of type FIXED_RECORD are only supported in location \"global\".")
		}
	case "PER_PROJECT_RECORD":
	default:
		return apierr.InvalidArgument("Invalid value for type: %q.", t)
	}
	res["type"] = t
	res["dnsResourceRecord"] = mustObj(dnsRecord(n.Project, d, t))
	return nil
}

// dnsRecord returns the CNAME a DNS authorization asks for. It is
// deterministic per project, domain and type, like the real service, so a
// re-created authorization keeps working with an existing record.
func dnsRecord(project, domain, typ string) *cmv1.DnsResourceRecord {
	sum := sha256.Sum256([]byte(project + "|" + domain + "|" + typ))
	h := hex.EncodeToString(sum[:16])
	uid := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	name := "_acme-challenge." + domain + "."
	if typ == "PER_PROJECT_RECORD" {
		ps := sha256.Sum256([]byte(project))
		name = "_acme-challenge_" + base32Lower(ps[:10]) + "." + domain + "."
	}
	return &cmv1.DnsResourceRecord{
		Name: name,
		Type: "CNAME",
		Data: fmt.Sprintf("%s.%d.authorize.certificatemanager.goog.", uid, int(sum[16])%10),
	}
}

func base32Lower(b []byte) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var out strings.Builder
	var buf, bits uint
	for _, c := range b {
		buf = buf<<8 | uint(c)
		bits += 8
		for bits >= 5 {
			out.WriteByte(alphabet[(buf>>(bits-5))&31])
			bits -= 5
		}
	}
	return out.String()
}

// prepareIssuance validates a certificate issuance config.
func prepareIssuance(res obj) error {
	c, err := as[cmv1.CertificateIssuanceConfig](res)
	if err != nil {
		return err
	}
	if c.CertificateAuthorityConfig == nil || c.CertificateAuthorityConfig.CertificateAuthorityServiceConfig == nil ||
		c.CertificateAuthorityConfig.CertificateAuthorityServiceConfig.CaPool == "" {
		return apierr.InvalidArgument("Field certificate_authority_config.certificate_authority_service_config.ca_pool is required.")
	}
	if c.Lifetime == "" {
		return apierr.InvalidArgument("Field lifetime is required.")
	}
	if c.RotationWindowPercentage <= 0 || c.RotationWindowPercentage >= 100 {
		return apierr.InvalidArgument("Field rotation_window_percentage must be between 1 and 99.")
	}
	switch c.KeyAlgorithm {
	case "", "RSA_2048", "ECDSA_P256":
	default:
		return apierr.InvalidArgument("Invalid value for key_algorithm: %q.", c.KeyAlgorithm)
	}
	return nil
}

// ---- managed certificate provisioning ----

// resolver is the part of the dns service used for domain authorization.
type resolver interface {
	Resolve(ctx context.Context, name string, qtype uint16) ([]mdns.RR, error)
}

// ForwardingRuleIPs is optionally implemented by the "lb" service for
// load balancer authorization of managed certificates: the IP addresses of
// the project's forwarding rules.
type ForwardingRuleIPs interface {
	ForwardingRuleIPs(ctx context.Context, project string) []string
}

// caID identifies the current instance CA.
func (s *Service) caID() string {
	if s.env.CA == nil {
		return ""
	}
	sum := sha256.Sum256(s.env.CA.Certificate().Raw)
	return hex.EncodeToString(sum[:])
}

// reconcileCert advances a managed certificate: PROVISIONING → ACTIVE once
// authorized, and re-issues ACTIVE certificates after a CA rotation. It
// reports whether the stored certificate changed.
func (s *Service) reconcileCert(ctx context.Context, name string) bool {
	var o obj
	var rec keyRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		o = load(tx, name)
		_ = store.GetJSON(tx, nsKeys, name, &rec)
		return nil
	})
	if o == nil || o["managed"] == nil || s.env.CA == nil {
		return false
	}
	c, err := as[cmv1.Certificate](o)
	if err != nil {
		return false
	}
	m := c.Managed
	switch m.State {
	case "ACTIVE":
		if rec.Issuer == s.caID() {
			return false
		}
	case "PROVISIONING":
	default:
		return false
	}
	n, err := parseName(kCert, name)
	if err != nil {
		return false
	}
	now := s.now()
	authorized := true
	var attempts []*cmv1.AuthorizationAttemptInfo
	if m.State == "PROVISIONING" && m.IssuanceConfig == "" {
		for _, d := range m.Domains {
			a := s.authorize(ctx, n.Project, d, m.DnsAuthorizations)
			a.AttemptTime = now
			attempts = append(attempts, a)
			if a.State != "AUTHORIZED" {
				authorized = false
			}
		}
	} else {
		for _, d := range m.Domains {
			attempts = append(attempts, &cmv1.AuthorizationAttemptInfo{Domain: d, State: "AUTHORIZED", AttemptTime: now})
		}
	}
	upd := clone(o)
	var newRec *keyRecord
	if authorized {
		cert, err := s.env.CA.Issue(ca.Leaf{DNSNames: m.Domains, Client: c.Scope == "CLIENT_AUTH", Organization: "gcpemu"})
		if err != nil {
			s.env.Log.Error("certs: issue managed certificate", "certificate", name, "err", err)
			return false
		}
		chain, key, err := ca.EncodeCert(cert)
		if err != nil {
			return false
		}
		newRec = &keyRecord{Chain: string(chain), Key: string(key), Issuer: s.caID()}
		m.State = "ACTIVE"
		m.ProvisioningIssue = nil
		upd["pemCertificate"] = string(chain)
		upd["expireTime"] = cert.Leaf.NotAfter.UTC().Format(time.RFC3339)
		upd["sanDnsnames"] = toAny(m.Domains)
	} else {
		m.ProvisioningIssue = &cmv1.ProvisioningIssue{
			Reason:  "AUTHORIZATION_ISSUE",
			Details: "Authorization attempt failed for one or more domains; see authorizationAttemptInfo for details.",
		}
	}
	m.AuthorizationAttemptInfo = attempts
	upd["managed"] = mustObj(m)
	if !authorized && sameIgnoringAttemptTime(o["managed"], upd["managed"]) {
		return false
	}
	changed := false
	err = s.env.Store.Update(func(tx store.Tx) error {
		cur := load(tx, name)
		if cur == nil || !jsonEqual(cur["managed"], o["managed"]) || cur["pemCertificate"] != o["pemCertificate"] {
			return nil // changed concurrently; the next pass retries
		}
		if newRec != nil {
			if err := store.PutJSON(tx, nsKeys, name, newRec); err != nil {
				return err
			}
		}
		changed = true
		return put(tx, name, upd)
	})
	if err != nil {
		s.env.Log.Error("certs: update managed certificate", "certificate", name, "err", err)
		return false
	}
	if changed {
		s.invalidate()
	}
	return changed
}

func sameIgnoringAttemptTime(a, b any) bool {
	strip := func(v any) any {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if l, ok := m["authorizationAttemptInfo"].([]any); ok {
			for _, e := range l {
				if em, ok := e.(map[string]any); ok {
					delete(em, "attemptTime")
				}
			}
		}
		return m
	}
	return jsonEqual(strip(a), strip(b))
}

// authorize checks one domain of a managed certificate.
func (s *Service) authorize(ctx context.Context, project, domain string, dnsAuths []string) *cmv1.AuthorizationAttemptInfo {
	a := &cmv1.AuthorizationAttemptInfo{Domain: domain, State: "AUTHORIZING"}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	svc, _ := s.env.Lookup("dns")
	res, _ := svc.(resolver)
	base := strings.TrimPrefix(domain, "*.")
	if len(dnsAuths) > 0 {
		var rec *cmv1.DnsResourceRecord
		_ = s.env.Store.View(func(tx store.Tx) error {
			for _, an := range dnsAuths {
				o := load(tx, canonical(an))
				if o != nil && strings.EqualFold(str(o["domain"]), base) {
					rec, _ = as[cmv1.DnsResourceRecord](asObj(o["dnsResourceRecord"]))
					return nil
				}
			}
			return nil
		})
		if rec == nil {
			a.State, a.FailureReason = "FAILED", "CONFIG"
			a.Details = fmt.Sprintf("No DNS authorization covers %s.", domain)
			return a
		}
		ts := &cmv1.Troubleshooting{Cname: &cmv1.CNAME{Name: rec.Name, ExpectedData: rec.Data}}
		a.Troubleshooting = ts
		if res == nil {
			a.Details = "The emulated Cloud DNS service is not running; the authorization CNAME cannot be checked."
			return a
		}
		rrs, _ := res.Resolve(ctx, rec.Name, mdns.TypeCNAME)
		for _, rr := range rrs {
			if cn, ok := rr.(*mdns.CNAME); ok {
				ts.Cname.ResolvedData = append(ts.Cname.ResolvedData, cn.Target)
				if strings.EqualFold(mdns.Fqdn(cn.Target), mdns.Fqdn(rec.Data)) {
					a.State, a.Troubleshooting = "AUTHORIZED", nil
					return a
				}
			}
		}
		a.Details = fmt.Sprintf("The CNAME record %s does not resolve to %s.", rec.Name, rec.Data)
		ts.Issues = append(ts.Issues, "CNAME_MISMATCH")
		return a
	}
	// Load balancer authorization: the domain must resolve to the IP of a
	// forwarding rule of the project.
	ts := &cmv1.Troubleshooting{Ips: &cmv1.IPs{}}
	a.Troubleshooting = ts
	var serving []string
	if lb, ok := s.env.Lookup("lb"); ok {
		if fr, ok := lb.(ForwardingRuleIPs); ok {
			serving = fr.ForwardingRuleIPs(ctx, project)
		}
	}
	ts.Ips.Serving = serving
	if res != nil {
		for _, qt := range []uint16{mdns.TypeA, mdns.TypeAAAA} {
			rrs, _ := res.Resolve(ctx, domain, qt)
			for _, rr := range rrs {
				var ip net.IP
				switch r := rr.(type) {
				case *mdns.A:
					ip = r.A
				case *mdns.AAAA:
					ip = r.AAAA
				}
				if ip != nil {
					ts.Ips.Resolved = append(ts.Ips.Resolved, ip.String())
				}
			}
		}
	}
	for _, r := range ts.Ips.Resolved {
		for _, sv := range serving {
			if r == sv {
				a.State, a.Troubleshooting = "AUTHORIZED", nil
				return a
			}
		}
	}
	a.Details = fmt.Sprintf("%s does not resolve to the IP address of a load balancer forwarding rule in project %s.", domain, project)
	ts.Issues = append(ts.Issues, "WRONG_IP_ADDRESS")
	return a
}

func asObj(v any) obj {
	m, _ := v.(map[string]any)
	return m
}

// reconcileAll runs one provisioning pass over every managed certificate.
func (s *Service) reconcileAll(ctx context.Context) {
	var names []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, o := range scanAll(tx, kCert) {
			if st, _ := getPath(o, "managed.state"); st == "PROVISIONING" || st == "ACTIVE" {
				names = append(names, str(o["name"]))
			}
		}
		return nil
	})
	for _, n := range names {
		if ctx.Err() != nil {
			return
		}
		s.reconcileCert(ctx, n)
	}
}

// reconcileLoop provisions managed certificates in the background.
func (s *Service) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		s.reconcileAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kickC:
		}
	}
}

// kick schedules a provisioning pass.
func (s *Service) kick() {
	select {
	case s.kickC <- struct{}{}:
	default:
	}
}
