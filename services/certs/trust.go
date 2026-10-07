package certs

import (
	"crypto/x509"
	"encoding/pem"
	"strings"

	cmv1 "google.golang.org/api/certificatemanager/v1"
	nsv1 "google.golang.org/api/networksecurity/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Trust configs (FR-LB-006, FR-LB-009) and the Network Security resources
// that reference them.

// parseCerts parses every CERTIFICATE block of a PEM string.
func parseCerts(field, s string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := []byte(s)
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
			return nil, apierr.InvalidArgument("Invalid PEM certificate in %s: %v", field, err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, apierr.InvalidArgument("Invalid PEM certificate in %s: no CERTIFICATE block found.", field)
	}
	return out, nil
}

// prepareTrust validates a trust config.
func prepareTrust(res obj) error {
	t, err := as[cmv1.TrustConfig](res)
	if err != nil {
		return err
	}
	if len(t.TrustStores) > 1 {
		return apierr.InvalidArgument("Only one trust store is supported.")
	}
	for _, ts := range t.TrustStores {
		for _, a := range ts.TrustAnchors {
			if _, err := parseCerts("trust_stores.trust_anchors.pem_certificate", a.PemCertificate); err != nil {
				return err
			}
		}
		for _, a := range ts.IntermediateCas {
			if _, err := parseCerts("trust_stores.intermediate_cas.pem_certificate", a.PemCertificate); err != nil {
				return err
			}
		}
	}
	for _, a := range t.AllowlistedCertificates {
		if _, err := parseCerts("allowlisted_certificates.pem_certificate", a.PemCertificate); err != nil {
			return err
		}
	}
	return nil
}

// trustCerts returns the effective verification roots of a trust config:
// its trust anchors, the intermediate CAs that chain to them (so leaves
// signed by an intermediate verify without the peer sending it) and the
// allowlisted certificates (accepted as-is).
func trustCerts(o obj) []*x509.Certificate {
	t, err := as[cmv1.TrustConfig](o)
	if err != nil {
		return nil
	}
	var out, inters []*x509.Certificate
	anchors := x509.NewCertPool()
	inter := x509.NewCertPool()
	for _, ts := range t.TrustStores {
		for _, a := range ts.TrustAnchors {
			cs, _ := parseCerts("", a.PemCertificate)
			for _, c := range cs {
				anchors.AddCert(c)
				out = append(out, c)
			}
		}
		for _, a := range ts.IntermediateCas {
			cs, _ := parseCerts("", a.PemCertificate)
			for _, c := range cs {
				inter.AddCert(c)
				inters = append(inters, c)
			}
		}
	}
	for _, c := range inters {
		if _, err := c.Verify(x509.VerifyOptions{Roots: anchors, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
			out = append(out, c)
		}
	}
	for _, a := range t.AllowlistedCertificates {
		cs, _ := parseCerts("", a.PemCertificate)
		out = append(out, cs...)
	}
	return out
}

// ---- Network Security ----

// prepareBAC validates a backend authentication config.
func (s *Service) prepareBAC(n resName, res obj) error {
	b, err := as[nsv1.BackendAuthenticationConfig](res)
	if err != nil {
		return err
	}
	switch b.WellKnownRoots {
	case "", "NONE", "PUBLIC_ROOTS":
	default:
		return apierr.InvalidArgument("Invalid value for well_known_roots: %q.", b.WellKnownRoots)
	}
	if b.ClientCertificate != "" {
		cn, err := parseName(kCert, b.ClientCertificate)
		if err != nil {
			return err
		}
		o, _, err := s.loadKind(kCert, b.ClientCertificate)
		if err != nil {
			return apierr.InvalidArgument("Certificate %s not found.", cn.name(kCert))
		}
		if str(o["scope"]) != "CLIENT_AUTH" {
			return apierr.InvalidArgument("Certificate %s must have scope CLIENT_AUTH to be used as a client certificate.", cn.name(kCert))
		}
		res["clientCertificate"] = cn.name(kCert)
	}
	if b.TrustConfig != "" {
		tn, err := parseName(kTrust, b.TrustConfig)
		if err != nil {
			return err
		}
		if _, _, err := s.loadKind(kTrust, b.TrustConfig); err != nil {
			return apierr.InvalidArgument("Trust config %s not found.", tn.name(kTrust))
		}
		res["trustConfig"] = tn.name(kTrust)
	} else if b.WellKnownRoots != "PUBLIC_ROOTS" {
		return apierr.InvalidArgument("Field trust_config is required unless well_known_roots is PUBLIC_ROOTS.")
	}
	return nil
}

// prepareServerTLS validates a server TLS policy.
func (s *Service) prepareServerTLS(res obj) error {
	p, err := as[nsv1.ServerTlsPolicy](res)
	if err != nil {
		return err
	}
	m := p.MtlsPolicy
	if m == nil {
		return nil
	}
	switch m.ClientValidationMode {
	case "", "ALLOW_INVALID_OR_MISSING_CLIENT_CERT", "REJECT_INVALID":
	default:
		return apierr.InvalidArgument("Invalid value for mtls_policy.client_validation_mode: %q.", m.ClientValidationMode)
	}
	if m.ClientValidationTrustConfig != "" {
		tn, err := parseName(kTrust, m.ClientValidationTrustConfig)
		if err != nil {
			return err
		}
		if _, _, err := s.loadKind(kTrust, m.ClientValidationTrustConfig); err != nil {
			return apierr.InvalidArgument("Trust config %s not found.", tn.name(kTrust))
		}
		setPath(res, "mtlsPolicy.clientValidationTrustConfig", tn.name(kTrust), true)
	} else if m.ClientValidationMode == "REJECT_INVALID" {
		return apierr.InvalidArgument("mtls_policy.client_validation_trust_config is required when client_validation_mode is REJECT_INVALID.")
	}
	return nil
}

// refersTo reports whether a stored reference names target.
func refersTo(ref, target string) bool {
	return ref != "" && strings.EqualFold(canonical(ref), target)
}
