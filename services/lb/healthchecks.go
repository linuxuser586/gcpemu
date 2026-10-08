package lb

import (
	"context"
	"strings"

	computev1 "google.golang.org/api/compute/v1"
)

// Health checks (healthChecks and regionHealthChecks; FR-LB-001, FR-LB-007).

var kindHealthCheck = &kind{
	coll: "healthChecks", typ: "compute#healthCheck", snake: "health_check",
	gperm: "compute.healthChecks", rperm: "compute.regionHealthChecks",
	aggKind: "compute#healthChecksAggregatedList",
	newObj:  func() any { return &computev1.HealthCheck{} },
}

func prepareHealthCheck(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	h := obj.(*computev1.HealthCheck)
	blocks := map[string]bool{
		"HTTP": h.HttpHealthCheck != nil, "HTTPS": h.HttpsHealthCheck != nil, "HTTP2": h.Http2HealthCheck != nil,
		"TCP": h.TcpHealthCheck != nil, "SSL": h.SslHealthCheck != nil, "GRPC": h.GrpcHealthCheck != nil,
		"GRPC_WITH_TLS": h.GrpcTlsHealthCheck != nil,
	}
	if h.Type == "" {
		for t, set := range blocks {
			if set {
				h.Type = t
			}
		}
	}
	set, ok := blocks[h.Type]
	if !ok {
		return errInvalid("resource.type", h.Type, "Must be one of HTTP, HTTPS, HTTP2, TCP, SSL, GRPC or GRPC_WITH_TLS.")
	}
	n := 0
	for _, v := range blocks {
		if v {
			n++
		}
	}
	if n > 1 {
		return errInvalid("resource", h.Name, "Exactly one health check protocol block may be specified.")
	}
	if h.CheckIntervalSec == 0 {
		h.CheckIntervalSec = 5
	}
	if h.TimeoutSec == 0 {
		h.TimeoutSec = 5
	}
	if h.HealthyThreshold == 0 {
		h.HealthyThreshold = 2
	}
	if h.UnhealthyThreshold == 0 {
		h.UnhealthyThreshold = 2
	}
	if h.CheckIntervalSec < 1 || h.CheckIntervalSec > 300 {
		return errInvalid("resource.checkIntervalSec", h.CheckIntervalSec, "Must be between 1 and 300.")
	}
	if h.TimeoutSec > h.CheckIntervalSec {
		return errInvalid("resource.timeoutSec", h.TimeoutSec, "Timeout sec must be less than or equal to check interval sec.")
	}
	if h.HealthyThreshold < 1 || h.HealthyThreshold > 10 || h.UnhealthyThreshold < 1 || h.UnhealthyThreshold > 10 {
		return errInvalid("resource.healthyThreshold", h.HealthyThreshold, "Thresholds must be between 1 and 10.")
	}
	defPort := int64(80)
	switch h.Type {
	case "HTTPS", "HTTP2", "SSL":
		defPort = 443
	case "GRPC", "GRPC_WITH_TLS":
		defPort = 0
	}
	fixPort := func(port *int64, spec *string, proxy *string, path *string) error {
		switch *spec {
		case "", "USE_FIXED_PORT":
			if *port == 0 {
				*port = defPort
			}
		case "USE_SERVING_PORT", "USE_NAMED_PORT":
			if *port != 0 && *spec == "USE_SERVING_PORT" {
				return errInvalid("resource.port", *port, "Port must not be set with USE_SERVING_PORT.")
			}
		default:
			return errInvalid("resource.portSpecification", *spec, "")
		}
		if *port < 0 || *port > 65535 {
			return errInvalid("resource.port", *port, "Must be between 1 and 65535.")
		}
		if proxy != nil && *proxy == "" {
			*proxy = "NONE"
		}
		if path != nil && *path == "" {
			*path = "/"
		}
		return nil
	}
	if !set {
		// The type names a block that was not given: GCP fills defaults.
		switch h.Type {
		case "HTTP":
			h.HttpHealthCheck = &computev1.HTTPHealthCheck{}
		case "HTTPS":
			h.HttpsHealthCheck = &computev1.HTTPSHealthCheck{}
		case "HTTP2":
			h.Http2HealthCheck = &computev1.HTTP2HealthCheck{}
		case "TCP":
			h.TcpHealthCheck = &computev1.TCPHealthCheck{}
		case "SSL":
			h.SslHealthCheck = &computev1.SSLHealthCheck{}
		case "GRPC":
			h.GrpcHealthCheck = &computev1.GRPCHealthCheck{}
		case "GRPC_WITH_TLS":
			h.GrpcTlsHealthCheck = &computev1.GRPCTLSHealthCheck{}
		}
	}
	var err error
	switch {
	case h.HttpHealthCheck != nil:
		c := h.HttpHealthCheck
		err = fixPort(&c.Port, &c.PortSpecification, &c.ProxyHeader, &c.RequestPath)
	case h.HttpsHealthCheck != nil:
		c := h.HttpsHealthCheck
		err = fixPort(&c.Port, &c.PortSpecification, &c.ProxyHeader, &c.RequestPath)
	case h.Http2HealthCheck != nil:
		c := h.Http2HealthCheck
		err = fixPort(&c.Port, &c.PortSpecification, &c.ProxyHeader, &c.RequestPath)
	case h.TcpHealthCheck != nil:
		c := h.TcpHealthCheck
		err = fixPort(&c.Port, &c.PortSpecification, &c.ProxyHeader, nil)
	case h.SslHealthCheck != nil:
		c := h.SslHealthCheck
		err = fixPort(&c.Port, &c.PortSpecification, &c.ProxyHeader, nil)
	case h.GrpcHealthCheck != nil:
		c := h.GrpcHealthCheck
		err = fixPort(&c.Port, &c.PortSpecification, nil, nil)
	case h.GrpcTlsHealthCheck != nil:
		c := h.GrpcTlsHealthCheck
		err = fixPort(&c.Port, &c.PortSpecification, nil, nil)
	}
	return err
}

// Legacy HTTP and HTTPS health checks (httpHealthChecks and
// httpsHealthChecks, global only). Only target pools use them, which the
// emulator does not have, so they are recorded and never probe.

var kindHTTPHealthCheck = &kind{
	coll: "httpHealthChecks", typ: "compute#httpHealthCheck", snake: "http_health_check",
	gperm:  "compute.httpHealthChecks",
	newObj: func() any { return &computev1.HttpHealthCheck{} },
}

var kindHTTPSHealthCheck = &kind{
	coll: "httpsHealthChecks", typ: "compute#httpsHealthCheck", snake: "https_health_check",
	gperm:  "compute.httpsHealthChecks",
	newObj: func() any { return &computev1.HttpsHealthCheck{} },
}

// legacyHealthCheck holds the fields the two legacy kinds share.
type legacyHealthCheck struct {
	port, interval, timeout, healthy, unhealthy *int64
	path                                        *string
}

func prepareLegacyHealthCheck(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	var h legacyHealthCheck
	defPort := int64(80)
	switch c := obj.(type) {
	case *computev1.HttpHealthCheck:
		h = legacyHealthCheck{&c.Port, &c.CheckIntervalSec, &c.TimeoutSec, &c.HealthyThreshold, &c.UnhealthyThreshold, &c.RequestPath}
	case *computev1.HttpsHealthCheck:
		h = legacyHealthCheck{&c.Port, &c.CheckIntervalSec, &c.TimeoutSec, &c.HealthyThreshold, &c.UnhealthyThreshold, &c.RequestPath}
		defPort = 443
	}
	for _, d := range []struct {
		v   *int64
		def int64
	}{{h.port, defPort}, {h.interval, 5}, {h.timeout, 5}, {h.healthy, 2}, {h.unhealthy, 2}} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if *h.path == "" {
		*h.path = "/"
	}
	if *h.port < 1 || *h.port > 65535 {
		return errInvalid("resource.port", *h.port, "Must be between 1 and 65535.")
	}
	if *h.interval < 1 || *h.interval > 300 {
		return errInvalid("resource.checkIntervalSec", *h.interval, "Must be between 1 and 300.")
	}
	if *h.timeout > *h.interval {
		return errInvalid("resource.timeoutSec", *h.timeout, "Timeout sec must be less than or equal to check interval sec.")
	}
	if *h.healthy < 1 || *h.healthy > 10 || *h.unhealthy < 1 || *h.unhealthy > 10 {
		return errInvalid("resource.healthyThreshold", *h.healthy, "Thresholds must be between 1 and 10.")
	}
	if !strings.HasPrefix(*h.path, "/") {
		return errInvalid("resource.requestPath", *h.path, "Must start with '/'.")
	}
	return nil
}
