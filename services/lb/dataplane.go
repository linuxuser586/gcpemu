package lb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/services/cdn"
	"github.com/linuxuser586/gcpemu/services/lb/urlmap"
)

// The data plane: one listener per forwarding rule (FR-LB-002) serving a
// frontend built from the forwarding rule, its target proxy and URL map.
// Configuration is an immutable snapshot rebuilt after every committed
// mutation (reconcile); per-backend runtime state (endpoints, health,
// load-balancing counters) lives in backendSvc values that survive
// rebuilds.

type dataplane struct {
	s *Service

	cfg atomic.Pointer[config]

	mu        sync.Mutex
	listeners map[string]*listener // by frontend key "ip:port"
	loopX     map[string]int       // forwarding rule IP → 127.0.0.x
	nextPort  int
	backends  map[string]*backendSvc // by backend service path
	ctx       context.Context
	closed    bool

	logs *accessLogger
	edge *edge
}

// config is a snapshot of the load-balancing resources.
type config struct {
	fronts   map[string]*frontend
	byIP     map[string]bool // forwarding rule IPs
	svcs     map[string]*backendSvc
	buckets  map[string]*computev1.BackendBucket
	frByPath map[string]*computev1.ForwardingRule
}

// frontend is one forwarding rule's serving configuration.
type frontend struct {
	key     string // "ip:port"
	fr      *computev1.ForwardingRule
	frPath  string
	ip      string
	port    int
	https   bool
	project string
	number  string
	region  string // "" for global

	proxyPath string
	urlMap    string
	router    *urlmap.Router
	routeErr  string

	// TLS (HTTPS proxies).
	certs     []*tls.Certificate
	certMap   string
	policy    *computev1.SslPolicy
	mtlsRoots *x509.CertPool
	mtlsMode  string
	keepAlive time.Duration
}

func newDataplane(s *Service) *dataplane {
	d := &dataplane{s: s, listeners: map[string]*listener{}, loopX: map[string]int{}, backends: map[string]*backendSvc{}}
	d.cfg.Store(&config{fronts: map[string]*frontend{}, byIP: map[string]bool{}, svcs: map[string]*backendSvc{}})
	return d
}

func (d *dataplane) start(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.closed = false
	d.mu.Unlock()
	d.logs = newAccessLogger(d.s.env)
	d.edge = newEdge(d)
	go d.endpointLoop(ctx)
	d.installDNSMapper()
}

func (d *dataplane) close(ctx context.Context) {
	d.mu.Lock()
	d.closed = true
	ls := d.listeners
	d.listeners = map[string]*listener{}
	bs := d.backends
	d.backends = map[string]*backendSvc{}
	d.mu.Unlock()
	for _, l := range ls {
		l.close()
	}
	for _, b := range bs {
		b.stop()
	}
	if d.edge != nil {
		d.edge.close(ctx)
	}
	if d.logs != nil {
		d.logs.close()
	}
}

// reconcile rebuilds the configuration snapshot and adjusts listeners and
// backend state to it.
func (d *dataplane) reconcile() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ctx == nil {
		return
	}
	s := d.s
	cfg := &config{fronts: map[string]*frontend{}, byIP: map[string]bool{}, svcs: map[string]*backendSvc{},
		buckets: map[string]*computev1.BackendBucket{}, frByPath: map[string]*computev1.ForwardingRule{}}

	// Backend services: keep runtime state, update configuration.
	for _, obj := range s.loadAll(kindBackendService, "") {
		b := obj.(*computev1.BackendService)
		p := relPath(b.SelfLink)
		bs := d.backends[p]
		if bs == nil {
			bs = newBackendSvc(d, p)
			d.backends[p] = bs
		}
		bs.configure(b)
		cfg.svcs[p] = bs
	}
	for p, bs := range d.backends {
		if cfg.svcs[p] == nil {
			bs.stop()
			delete(d.backends, p)
		}
	}
	for _, obj := range s.loadAll(kindBackendBucket, "") {
		b := obj.(*computev1.BackendBucket)
		cfg.buckets[relPath(b.SelfLink)] = b
	}

	// Frontends.
	routers := map[string]*urlmap.Router{}
	routeErrs := map[string]string{}
	for _, obj := range s.loadAll(kindForwardingRule, "") {
		f := obj.(*computev1.ForwardingRule)
		fe := d.buildFrontend(f, routers, routeErrs)
		if fe == nil {
			continue
		}
		cfg.fronts[fe.key] = fe
		cfg.byIP[fe.ip] = true
		cfg.frByPath[fe.frPath] = f
	}
	d.cfg.Store(cfg)

	// Listeners.
	for key, l := range d.listeners {
		fe := cfg.fronts[key]
		if fe == nil || fe.https != l.https || fe.fr.Name != l.name {
			l.close()
			delete(d.listeners, key)
		}
	}
	for key, fe := range cfg.fronts {
		if _, ok := d.listeners[key]; ok {
			continue
		}
		l, err := d.listen(fe)
		if err != nil {
			s.env.Log.Warn("lb: cannot listen for forwarding rule", "forwardingRule", fe.frPath, "err", err)
			continue
		}
		d.listeners[key] = l
	}
}

// buildFrontend resolves a forwarding rule's target proxy, URL map and TLS
// material. It returns nil for rules that cannot serve (missing target).
func (d *dataplane) buildFrontend(f *computev1.ForwardingRule, routers map[string]*urlmap.Router, routeErrs map[string]string) *frontend {
	s := d.s
	frPath := relPath(f.SelfLink)
	sc := scopeOfPath(frPath)
	port := 0
	if lo, _, ok := strings.Cut(f.PortRange, "-"); ok {
		port = atoiSafe(lo)
	}
	fe := &frontend{
		key: net.JoinHostPort(f.IPAddress, itoa(port)), fr: f, frPath: frPath, ip: f.IPAddress, port: port,
		project: sc.project, number: project.NumberString(sc.project), region: sc.region,
		proxyPath: relPath(f.Target), keepAlive: 610 * time.Second,
	}
	switch collOf(fe.proxyPath) {
	case "targetHttpProxies":
		obj, ok := s.load(kindTargetHTTPProxy, fe.proxyPath)
		if !ok {
			return nil
		}
		p := obj.(*computev1.TargetHttpProxy)
		fe.urlMap = relPath(p.UrlMap)
		if p.HttpKeepAliveTimeoutSec > 0 {
			fe.keepAlive = time.Duration(p.HttpKeepAliveTimeoutSec) * time.Second
		}
	case "targetHttpsProxies":
		obj, ok := s.load(kindTargetHTTPSProxy, fe.proxyPath)
		if !ok {
			return nil
		}
		p := obj.(*computev1.TargetHttpsProxy)
		fe.https = true
		fe.urlMap = relPath(p.UrlMap)
		if p.HttpKeepAliveTimeoutSec > 0 {
			fe.keepAlive = time.Duration(p.HttpKeepAliveTimeoutSec) * time.Second
		}
		for _, c := range p.SslCertificates {
			cert, _, err := s.tlsCertificate(relPath(c))
			if err != nil {
				s.env.Log.Warn("lb: SSL certificate unusable", "certificate", c, "err", err)
			}
			if cert != nil {
				fe.certs = append(fe.certs, cert)
			}
		}
		fe.certMap = p.CertificateMap
		if p.SslPolicy != "" {
			if o, ok := s.load(kindSSLPolicy, relPath(p.SslPolicy)); ok {
				fe.policy = o.(*computev1.SslPolicy)
			}
		}
		if p.ServerTlsPolicy != "" {
			if cm := s.certManager(); cm != nil {
				roots, mode, err := cm.ServerTLSPolicy(context.Background(), strings.TrimPrefix(p.ServerTlsPolicy, netSecPrefix))
				if err != nil {
					s.env.Log.Warn("lb: server TLS policy unusable", "policy", p.ServerTlsPolicy, "err", err)
				}
				fe.mtlsRoots, fe.mtlsMode = roots, mode
			}
		}
	default:
		return nil
	}
	if r, ok := routers[fe.urlMap]; ok {
		fe.router, fe.routeErr = r, routeErrs[fe.urlMap]
	} else {
		if obj, ok := s.load(kindURLMap, fe.urlMap); ok {
			r, err := urlmap.Compile(obj.(*computev1.UrlMap))
			if err != nil {
				routeErrs[fe.urlMap] = err.Error()
			}
			routers[fe.urlMap] = r
			fe.router, fe.routeErr = r, routeErrs[fe.urlMap]
		}
	}
	return fe
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// certManager returns the Certificate Manager / Network Security provider.
func (s *Service) certManager() emu.CertManager {
	if svc, ok := s.env.Lookup("certs"); ok {
		if cm, ok := svc.(emu.CertManager); ok {
			return cm
		}
	}
	return nil
}

// cdnCache returns the Cloud CDN cache, or nil when cdn is not running.
func (s *Service) cdnCache() *cdn.Cache {
	if svc, ok := s.env.Lookup("cdn"); ok {
		if c, ok := svc.(interface{ Cache() *cdn.Cache }); ok {
			return c.Cache()
		}
	}
	return nil
}
