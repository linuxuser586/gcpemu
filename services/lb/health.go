package lb

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/services/compute"
)

// Active health checks (FR-LB-007): each backend service with a health
// check probes its endpoints every checkIntervalSec with timeoutSec,
// marking them HEALTHY after healthyThreshold consecutive successes and
// UNHEALTHY after unhealthyThreshold failures. A newly discovered endpoint
// is probed at once and its first result decides its initial state, so
// scaled-up pods receive traffic within seconds (FR-INT-001).

func (b *backendSvc) healthLoop(ctx context.Context) {
	var last time.Time
	for {
		hc := b.hcfg.Load()
		interval := time.Second
		if hc != nil {
			interval = time.Duration(hc.CheckIntervalSec) * time.Second
		}
		wait := interval - time.Since(last)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		onlyNew := false
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-b.hcKick:
			timer.Stop()
			onlyNew = true
		}
		hc = b.hcfg.Load()
		if hc == nil {
			continue
		}
		if !onlyNew {
			last = time.Now()
		}
		b.probeAll(ctx, hc, onlyNew)
	}
}

// probeAll probes endpoints concurrently (only unknown ones when onlyNew).
func (b *backendSvc) probeAll(ctx context.Context, hc *computev1.HealthCheck, onlyNew bool) {
	var wg sync.WaitGroup
	for _, e := range *b.eps.Load() {
		if onlyNew && e.state() != stateUnknown {
			continue
		}
		if e.draining.Load() {
			continue
		}
		wg.Add(1)
		go func(e *endpoint) {
			defer wg.Done()
			ok := b.probe(ctx, hc, e)
			b.record(hc, e, ok)
		}(e)
	}
	wg.Wait()
}

func (b *backendSvc) record(hc *computev1.HealthCheck, e *endpoint, ok bool) {
	if ok {
		e.fails.Store(0)
		n := e.succ.Add(1)
		if e.state() == stateUnknown || int64(n) >= hc.HealthyThreshold {
			e.health.Store(int32(stateHealthy))
		}
		return
	}
	e.succ.Store(0)
	n := e.fails.Add(1)
	if e.state() == stateUnknown || int64(n) >= hc.UnhealthyThreshold {
		e.health.Store(int32(stateUnhealthy))
	}
}

// probe runs one health check against an endpoint.
func (b *backendSvc) probe(ctx context.Context, hc *computev1.HealthCheck, e *endpoint) bool {
	timeout := time.Duration(hc.TimeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	portOf := func(port int64, spec string) int {
		if spec == "USE_SERVING_PORT" || spec == "USE_NAMED_PORT" || port == 0 {
			return e.port
		}
		return int(port)
	}
	switch {
	case hc.HttpHealthCheck != nil:
		c := hc.HttpHealthCheck
		return b.probeHTTP(ctx, e, portOf(c.Port, c.PortSpecification), "http", c.Host, c.RequestPath, c.Response, c.ProxyHeader)
	case hc.HttpsHealthCheck != nil:
		c := hc.HttpsHealthCheck
		return b.probeHTTP(ctx, e, portOf(c.Port, c.PortSpecification), "https", c.Host, c.RequestPath, c.Response, c.ProxyHeader)
	case hc.Http2HealthCheck != nil:
		c := hc.Http2HealthCheck
		return b.probeHTTP(ctx, e, portOf(c.Port, c.PortSpecification), "h2", c.Host, c.RequestPath, c.Response, c.ProxyHeader)
	case hc.TcpHealthCheck != nil:
		c := hc.TcpHealthCheck
		return b.probeTCP(ctx, e, portOf(c.Port, c.PortSpecification), false, c.Request, c.Response)
	case hc.SslHealthCheck != nil:
		c := hc.SslHealthCheck
		return b.probeTCP(ctx, e, portOf(c.Port, c.PortSpecification), true, c.Request, c.Response)
	case hc.GrpcHealthCheck != nil:
		c := hc.GrpcHealthCheck
		return b.probeTCP(ctx, e, portOf(c.Port, c.PortSpecification), false, "", "")
	case hc.GrpcTlsHealthCheck != nil:
		c := hc.GrpcTlsHealthCheck
		return b.probeTCP(ctx, e, portOf(c.Port, c.PortSpecification), true, "", "")
	}
	return false
}

func (b *backendSvc) probeHTTP(ctx context.Context, e *endpoint, port int, mode, host, path, want, proxyHdr string) bool {
	addr := net.JoinHostPort(e.ip, strconv.Itoa(port))
	if path == "" {
		path = "/"
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			c, err := b.d.dialContext(ctx, network, addr)
			if err == nil && proxyHdr == "PROXY_V1" {
				_, err = io.WriteString(c, "PROXY TCP4 "+b.d.hcSourceIP()+" "+e.ip+" 0 "+strconv.Itoa(port)+"\r\n")
			}
			return c, err
		},
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}
	defer t.CloseIdleConnections()
	scheme := "http"
	switch mode {
	case "https":
		scheme = "https"
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	case "h2":
		scheme = "https"
		t.ForceAttemptHTTP2 = true
		t.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+path, nil)
	if err != nil {
		return false
	}
	if host != "" {
		req.Host = host
	}
	req.Header.Set("User-Agent", "GoogleHC/1.0")
	resp, err := t.RoundTrip(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if want != "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return strings.HasPrefix(string(body), want)
	}
	return true
}

func (b *backendSvc) probeTCP(ctx context.Context, e *endpoint, port int, useTLS bool, request, want string) bool {
	c, err := b.d.dialContext(ctx, "tcp", net.JoinHostPort(e.ip, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if useTLS {
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
		if err := tc.HandshakeContext(ctx); err != nil {
			return false
		}
		c = tc
	}
	if request != "" {
		if _, err := io.WriteString(c, request); err != nil {
			return false
		}
	}
	if want != "" {
		buf := make([]byte, len(want))
		if _, err := io.ReadFull(bufio.NewReader(c), buf); err != nil {
			return false
		}
		return string(buf) == want
	}
	return true
}

// hcSourceIP is the address health check probes come from (PROXY headers).
func (d *dataplane) hcSourceIP() string { return "35.191.0.1" }

// groupHealth reports per-endpoint health of one backend group for
// backendServices.getHealth.
func (d *dataplane) groupHealth(ctx context.Context, bsPath, group string) []*computev1.HealthStatus {
	cfg := d.cfg.Load()
	b := cfg.svcs[bsPath]
	if b == nil {
		return nil
	}
	b.refreshEndpoints(ctx)
	var fr *computev1.ForwardingRule
	for _, f := range d.frontsUsing(bsPath) {
		fr = f
		break
	}
	hasHC := b.hcfg.Load() != nil
	var out []*computev1.HealthStatus
	for _, e := range *b.eps.Load() {
		if e.group != group {
			continue
		}
		st := "HEALTHY"
		switch {
		case e.draining.Load():
			st = "DRAINING"
		case !hasHC:
		case e.state() == stateUnknown:
			st = "UNKNOWN"
		case e.state() == stateUnhealthy:
			st = "UNHEALTHY"
		}
		hs := &computev1.HealthStatus{IpAddress: e.ip, Port: int64(e.port), HealthState: st}
		if e.instance != "" {
			sc := scopeOfPath(group)
			hs.Instance = compute.SelfLink("projects/" + sc.project + "/zones/" + e.zone + "/instances/" + e.instance)
		}
		if fr != nil {
			hs.ForwardingRule, hs.ForwardingRuleIp = fr.SelfLink, fr.IPAddress
		}
		out = append(out, hs)
	}
	return out
}

// frontsUsing lists forwarding rules whose URL map routes to a backend.
func (d *dataplane) frontsUsing(backend string) []*computev1.ForwardingRule {
	cfg := d.cfg.Load()
	var out []*computev1.ForwardingRule
	for _, fe := range cfg.fronts {
		obj, ok := d.s.load(kindURLMap, fe.urlMap)
		if !ok {
			continue
		}
		for _, r := range urlMapRefs(obj) {
			if relPath(r) == backend {
				out = append(out, fe.fr)
				break
			}
		}
	}
	return out
}
