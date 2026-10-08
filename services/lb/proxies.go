package lb

import (
	"context"
	"net/http"
	"strings"

	computev1 "google.golang.org/api/compute/v1"
)

// Target HTTP and HTTPS proxies (global and regional; FR-LB-001, FR-LB-004).

var kindTargetHTTPProxy = &kind{
	coll: "targetHttpProxies", typ: "compute#targetHttpProxy", snake: "target_http_proxy",
	gperm: "compute.targetHttpProxies", rperm: "compute.regionTargetHttpProxies",
	noUpdate: true,
	newObj:   func() any { return &computev1.TargetHttpProxy{} },
	refs:     func(obj any) []string { return []string{obj.(*computev1.TargetHttpProxy).UrlMap} },
}

var kindTargetHTTPSProxy = &kind{
	coll: "targetHttpsProxies", typ: "compute#targetHttpsProxy", snake: "target_https_proxy",
	gperm: "compute.targetHttpsProxies", rperm: "compute.regionTargetHttpsProxies",
	noUpdate: true,
	newObj:   func() any { return &computev1.TargetHttpsProxy{} },
	refs: func(obj any) []string {
		p := obj.(*computev1.TargetHttpsProxy)
		out := append([]string{p.UrlMap, p.SslPolicy}, p.SslCertificates...)
		return out
	},
}

const (
	certManagerPrefix = "//certificatemanager.googleapis.com/"
	netSecPrefix      = "//networksecurity.googleapis.com/"
)

func prepareHTTPSProxy(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	p := obj.(*computev1.TargetHttpsProxy)
	l, _, err := s.refExisting(sc, p.UrlMap, "resource.urlMap", kindURLMap)
	if err != nil {
		return err
	}
	p.UrlMap = l
	if err := s.prepareTLSRefs(sc, p.SslCertificates, &p.CertificateMap, &p.SslPolicy); err != nil {
		return err
	}
	if p.QuicOverride == "" {
		p.QuicOverride = "NONE"
	}
	switch p.QuicOverride {
	case "NONE", "ENABLE", "DISABLE":
	default:
		return errInvalid("resource.quicOverride", p.QuicOverride, "")
	}
	if p.ServerTlsPolicy != "" && !strings.HasPrefix(p.ServerTlsPolicy, "//") {
		p.ServerTlsPolicy = relPath(p.ServerTlsPolicy)
	}
	return nil
}

// prepareTLSRefs canonicalises the certificates, certificate map and SSL
// policy of a target HTTPS or SSL proxy.
func (s *Service) prepareTLSRefs(sc scope, certs []string, certMap, policy *string) error {
	if len(certs) > 15 {
		return errInvalid("resource.sslCertificates", len(certs), "At most 15 SSL certificates can be specified.")
	}
	seen := map[string]bool{}
	for i, c := range certs {
		cl, _, err := s.refExisting(sc, c, "resource.sslCertificates["+itoa(i)+"]", kindSSLCertificate)
		if err != nil {
			return err
		}
		if seen[cl] {
			return errInvalid("resource.sslCertificates", c, "Duplicate SSL certificate.")
		}
		seen[cl] = true
		certs[i] = cl
	}
	if *certMap != "" {
		if sc.region != "" {
			return errInvalid("resource.certificateMap", *certMap, "Certificate maps are only supported by global target proxies.")
		}
		cm := strings.TrimPrefix(*certMap, certManagerPrefix)
		cm = relPath(cm)
		segs := strings.Split(cm, "/")
		if len(segs) != 6 || segs[0] != "projects" || segs[2] != "locations" || segs[4] != "certificateMaps" {
			return errInvalid("resource.certificateMap", *certMap, "Must be of the form //certificatemanager.googleapis.com/projects/PROJECT/locations/global/certificateMaps/MAP.")
		}
		*certMap = certManagerPrefix + cm
	}
	if len(certs) == 0 && *certMap == "" {
		return errInvalid("resource.sslCertificates", "", "At least one SSL certificate or a certificate map must be specified.")
	}
	if *policy != "" {
		l, _, err := s.refExisting(sc, *policy, "resource.sslPolicy", kindSSLPolicy)
		if err != nil {
			return err
		}
		*policy = l
	}
	return nil
}

func (s *Service) httpProxyMethods() map[string]http.HandlerFunc {
	k := kindTargetHTTPProxy
	return map[string]http.HandlerFunc{
		"setUrlMap": s.mutateH(k, "setUrlMap", "setUrlMap",
			func() any { return &computev1.UrlMapReference{} },
			func(r *http.Request, sc scope, obj, req any) error {
				obj.(*computev1.TargetHttpProxy).UrlMap = req.(*computev1.UrlMapReference).UrlMap
				return nil
			}),
	}
}

func (s *Service) httpsProxyMethods() map[string]http.HandlerFunc {
	k := kindTargetHTTPSProxy
	px := func(o any) *computev1.TargetHttpsProxy { return o.(*computev1.TargetHttpsProxy) }
	return map[string]http.HandlerFunc{
		"setUrlMap": s.mutateH(k, "setUrlMap", "setUrlMap",
			func() any { return &computev1.UrlMapReference{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).UrlMap = req.(*computev1.UrlMapReference).UrlMap
				return nil
			}),
		"setSslCertificates": s.mutateH(k, "setSslCertificates", "setSslCertificates",
			func() any { return &computev1.TargetHttpsProxiesSetSslCertificatesRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).SslCertificates = req.(*computev1.TargetHttpsProxiesSetSslCertificatesRequest).SslCertificates
				return nil
			}),
		"setCertificateMap": s.mutateH(k, "setCertificateMap", "setCertificateMap",
			func() any { return &computev1.TargetHttpsProxiesSetCertificateMapRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).CertificateMap = req.(*computev1.TargetHttpsProxiesSetCertificateMapRequest).CertificateMap
				return nil
			}),
		"setSslPolicy": s.mutateH(k, "setSslPolicy", "setSslPolicy",
			func() any { return &computev1.SslPolicyReference{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).SslPolicy = req.(*computev1.SslPolicyReference).SslPolicy
				return nil
			}),
		"setQuicOverride": s.mutateH(k, "setQuicOverride", "setQuicOverride",
			func() any { return &computev1.TargetHttpsProxiesSetQuicOverrideRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).QuicOverride = req.(*computev1.TargetHttpsProxiesSetQuicOverrideRequest).QuicOverride
				return nil
			}),
	}
}

func prepareHTTPProxy(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	p := obj.(*computev1.TargetHttpProxy)
	l, _, err := s.refExisting(sc, p.UrlMap, "resource.urlMap", kindURLMap)
	if err != nil {
		return err
	}
	p.UrlMap = l
	return nil
}
