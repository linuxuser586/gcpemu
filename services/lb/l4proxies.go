package lb

import (
	"context"
	"net/http"

	computev1 "google.golang.org/api/compute/v1"
)

// Target SSL and TCP proxies (proxy Network Load Balancers) and target gRPC
// proxies (proxyless gRPC with Cloud Service Mesh). They are recorded: the
// emulator only proxies HTTP(S) (SRS 10.3), so forwarding rules that point
// at them get no listener.

var kindTargetSSLProxy = &kind{
	coll: "targetSslProxies", typ: "compute#targetSslProxy", snake: "target_ssl_proxy",
	gperm:   "compute.targetSslProxies",
	noPatch: true, noUpdate: true,
	newObj: func() any { return &computev1.TargetSslProxy{} },
	refs: func(obj any) []string {
		p := obj.(*computev1.TargetSslProxy)
		return append([]string{p.Service, p.SslPolicy}, p.SslCertificates...)
	},
}

var kindTargetTCPProxy = &kind{
	coll: "targetTcpProxies", typ: "compute#targetTcpProxy", snake: "target_tcp_proxy",
	gperm: "compute.targetTcpProxies", rperm: "compute.regionTargetTcpProxies",
	noPatch: true, noUpdate: true,
	newObj: func() any { return &computev1.TargetTcpProxy{} },
	refs:   func(obj any) []string { return []string{obj.(*computev1.TargetTcpProxy).Service} },
}

var kindTargetGRPCProxy = &kind{
	coll: "targetGrpcProxies", typ: "compute#targetGrpcProxy", snake: "target_grpc_proxy",
	gperm:    "compute.targetGrpcProxies",
	noUpdate: true,
	newObj:   func() any { return &computev1.TargetGrpcProxy{} },
	refs:     func(obj any) []string { return []string{obj.(*computev1.TargetGrpcProxy).UrlMap} },
}

// refL4Service canonicalises the backend service of a TCP or SSL proxy.
func (s *Service) refL4Service(sc scope, ref string) (string, error) {
	l, _, err := s.refExisting(sc, ref, "resource.service", kindBackendService)
	if err != nil {
		return "", err
	}
	obj, _ := s.load(kindBackendService, relPath(l))
	if p := obj.(*computev1.BackendService).Protocol; p != "TCP" && p != "SSL" {
		return "", errInvalid("resource.service", ref, "The backend service of a target TCP or SSL proxy must use protocol TCP or SSL.")
	}
	return l, nil
}

func checkProxyHeader(h *string) error {
	if *h == "" {
		*h = "NONE"
	}
	if *h != "NONE" && *h != "PROXY_V1" {
		return errInvalid("resource.proxyHeader", *h, "Must be NONE or PROXY_V1.")
	}
	return nil
}

func prepareSSLProxy(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	p := obj.(*computev1.TargetSslProxy)
	l, err := s.refL4Service(sc, p.Service)
	if err != nil {
		return err
	}
	p.Service = l
	if err := s.prepareTLSRefs(sc, p.SslCertificates, &p.CertificateMap, &p.SslPolicy); err != nil {
		return err
	}
	return checkProxyHeader(&p.ProxyHeader)
}

func prepareTCPProxy(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	p := obj.(*computev1.TargetTcpProxy)
	l, err := s.refL4Service(sc, p.Service)
	if err != nil {
		return err
	}
	p.Service = l
	return checkProxyHeader(&p.ProxyHeader)
}

func prepareGRPCProxy(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	p := obj.(*computev1.TargetGrpcProxy)
	l, _, err := s.refExisting(sc, p.UrlMap, "resource.urlMap", kindURLMap)
	if err != nil {
		return err
	}
	p.UrlMap = l
	p.SelfLinkWithId = ""
	return nil
}

func (s *Service) sslProxyMethods() map[string]http.HandlerFunc {
	k := kindTargetSSLProxy
	px := func(o any) *computev1.TargetSslProxy { return o.(*computev1.TargetSslProxy) }
	return map[string]http.HandlerFunc{
		"setBackendService": s.mutateH(k, "setBackendService", "setBackendService",
			func() any { return &computev1.TargetSslProxiesSetBackendServiceRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).Service = req.(*computev1.TargetSslProxiesSetBackendServiceRequest).Service
				return nil
			}),
		"setProxyHeader": s.mutateH(k, "setProxyHeader", "setProxyHeader",
			func() any { return &computev1.TargetSslProxiesSetProxyHeaderRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).ProxyHeader = req.(*computev1.TargetSslProxiesSetProxyHeaderRequest).ProxyHeader
				return nil
			}),
		"setSslCertificates": s.mutateH(k, "setSslCertificates", "setSslCertificates",
			func() any { return &computev1.TargetSslProxiesSetSslCertificatesRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).SslCertificates = req.(*computev1.TargetSslProxiesSetSslCertificatesRequest).SslCertificates
				return nil
			}),
		"setCertificateMap": s.mutateH(k, "setCertificateMap", "setCertificateMap",
			func() any { return &computev1.TargetSslProxiesSetCertificateMapRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).CertificateMap = req.(*computev1.TargetSslProxiesSetCertificateMapRequest).CertificateMap
				return nil
			}),
		"setSslPolicy": s.mutateH(k, "setSslPolicy", "setSslPolicy",
			func() any { return &computev1.SslPolicyReference{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).SslPolicy = req.(*computev1.SslPolicyReference).SslPolicy
				return nil
			}),
	}
}

func (s *Service) tcpProxyMethods() map[string]http.HandlerFunc {
	k := kindTargetTCPProxy
	px := func(o any) *computev1.TargetTcpProxy { return o.(*computev1.TargetTcpProxy) }
	return map[string]http.HandlerFunc{
		"setBackendService": s.mutateH(k, "update", "setBackendService",
			func() any { return &computev1.TargetTcpProxiesSetBackendServiceRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).Service = req.(*computev1.TargetTcpProxiesSetBackendServiceRequest).Service
				return nil
			}),
		"setProxyHeader": s.mutateH(k, "update", "setProxyHeader",
			func() any { return &computev1.TargetTcpProxiesSetProxyHeaderRequest{} },
			func(r *http.Request, sc scope, obj, req any) error {
				px(obj).ProxyHeader = req.(*computev1.TargetTcpProxiesSetProxyHeaderRequest).ProxyHeader
				return nil
			}),
	}
}
