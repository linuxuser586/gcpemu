package lb

import (
	"context"
	"crypto/tls"
	"net/http"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// SSL policies (global and regional; FR-LB-001): profile and minimum TLS
// version, applied by the proxy's TLS configuration (sslPolicyConfig).

var kindSSLPolicy = &kind{
	coll: "sslPolicies", typ: "compute#sslPolicy", snake: "ssl_policy",
	gperm: "compute.sslPolicies", rperm: "compute.regionSslPolicies",
	aggKind: "compute#sslPoliciesAggregatedList", listKind: "compute#sslPoliciesList",
	noUpdate: true,
	newObj:   func() any { return &computev1.SslPolicy{} },
	keep:     []string{"enabledFeatures", "warnings"},
}

// sslFeatures are GCP's SSL policy features (TLS 1.0-1.2 cipher suites).
var sslFeatures = []struct {
	name   string
	id     uint16
	modern bool
	strict bool // RESTRICTED
	fips   bool
}{
	{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, true, true, true},
	{"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, true, true, true},
	{"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256", tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, true, true, false},
	{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, true, true, true},
	{"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384", tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, true, true, true},
	{"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256", tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256, true, true, false},
	{"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA", tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA, true, false, false},
	{"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA", tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA, true, false, false},
	{"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA", tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA, true, false, false},
	{"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA", tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA, true, false, false},
	{"TLS_RSA_WITH_AES_128_GCM_SHA256", tls.TLS_RSA_WITH_AES_128_GCM_SHA256, false, false, false},
	{"TLS_RSA_WITH_AES_256_GCM_SHA384", tls.TLS_RSA_WITH_AES_256_GCM_SHA384, false, false, false},
	{"TLS_RSA_WITH_AES_128_CBC_SHA", tls.TLS_RSA_WITH_AES_128_CBC_SHA, false, false, false},
	{"TLS_RSA_WITH_AES_256_CBC_SHA", tls.TLS_RSA_WITH_AES_256_CBC_SHA, false, false, false},
	{"TLS_RSA_WITH_3DES_EDE_CBC_SHA", tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA, false, false, false},
}

var tlsVersions = map[string]uint16{
	"TLS_1_0": tls.VersionTLS10, "TLS_1_1": tls.VersionTLS11, "TLS_1_2": tls.VersionTLS12, "TLS_1_3": tls.VersionTLS13,
}

// enabledFeatures returns the features a profile enables.
func enabledFeatures(p *computev1.SslPolicy) []string {
	var out []string
	if p.Profile == "CUSTOM" {
		return append(out, p.CustomFeatures...)
	}
	for _, f := range sslFeatures {
		switch p.Profile {
		case "MODERN":
			if !f.modern {
				continue
			}
		case "RESTRICTED":
			if !f.strict {
				continue
			}
		case "FIPS_202205":
			if !f.fips {
				continue
			}
		}
		out = append(out, f.name)
	}
	return out
}

func prepareSSLPolicy(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	p := obj.(*computev1.SslPolicy)
	if p.Profile == "" {
		p.Profile = "COMPATIBLE"
	}
	if p.MinTlsVersion == "" {
		p.MinTlsVersion = "TLS_1_0"
	}
	if _, ok := tlsVersions[p.MinTlsVersion]; !ok {
		return errInvalid("resource.minTlsVersion", p.MinTlsVersion, "")
	}
	switch p.Profile {
	case "COMPATIBLE", "MODERN", "RESTRICTED", "FIPS_202205":
		if len(p.CustomFeatures) > 0 {
			return errInvalid("resource.customFeatures", p.CustomFeatures[0], "Custom features can only be specified with the CUSTOM profile.")
		}
	case "CUSTOM":
		if len(p.CustomFeatures) == 0 {
			return errInvalid("resource.customFeatures", "", "At least one custom feature is required with the CUSTOM profile.")
		}
		for _, cf := range p.CustomFeatures {
			if featureID(cf) == 0 {
				return errInvalid("resource.customFeatures", cf, "Unknown SSL feature.")
			}
		}
	default:
		return errInvalid("resource.profile", p.Profile, "")
	}
	p.EnabledFeatures = enabledFeatures(p)
	return nil
}

func featureID(name string) uint16 {
	for _, f := range sslFeatures {
		if f.name == name {
			return f.id
		}
	}
	return 0
}

// listAvailableFeatures implements sslPolicies.listAvailableFeatures.
func (s *Service) listAvailableFeatures(w http.ResponseWriter, r *http.Request) {
	sc, err := s.reqScope(r, kindSSLPolicy)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), kindSSLPolicy.perm(sc, "list"), "projects/"+sc.project); err != nil {
		apierr.Write(w, err)
		return
	}
	out := &computev1.SslPoliciesListAvailableFeaturesResponse{}
	for _, f := range sslFeatures {
		out.Features = append(out.Features, f.name)
	}
	writeJSON(w, http.StatusOK, out)
}

// applySSLPolicy restricts a server TLS config per the policy (nil = GCP
// default: COMPATIBLE, TLS 1.0).
func applySSLPolicy(cfg *tls.Config, p *computev1.SslPolicy) {
	if p == nil {
		cfg.MinVersion = tls.VersionTLS10
		return
	}
	cfg.MinVersion = tlsVersions[p.MinTlsVersion]
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS10
	}
	var ids []uint16
	for _, f := range p.EnabledFeatures {
		if id := featureID(f); id != 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		cfg.CipherSuites = ids
	}
}
