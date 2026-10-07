package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"sync"
	"sync/atomic"

	nsv1 "google.golang.org/api/networksecurity/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// emu.CertManager: the data-plane view of Certificate Manager and Network
// Security for the load balancer (FR-LB-004, FR-LB-006, FR-LB-009). These
// methods are called on TLS handshakes, so results are cached and the
// cache is dropped whenever any certs resource changes (generation
// counter). They perform no IAM checks: the load balancer is a data plane.

var _ emu.CertManager = (*Service)(nil)

// tlsCache memoises parsed certificates and map selections per generation.
type tlsCache struct {
	gen   atomic.Uint64
	mu    sync.Mutex
	at    uint64
	certs map[string]*tls.Certificate // certificate name → parsed key pair
	maps  map[string]*tls.Certificate // map + "|" + sni → selection (nil = none)
}

// invalidate drops every cached certificate and selection.
func (s *Service) invalidate() { s.cache.gen.Add(1) }

// cached returns the generation-checked cache maps (locked by the caller).
func (c *tlsCache) sync() {
	if g := c.gen.Load(); c.at != g || c.certs == nil {
		c.at, c.certs, c.maps = g, map[string]*tls.Certificate{}, map[string]*tls.Certificate{}
	}
}

// MapCertificate implements emu.CertManager.
func (s *Service) MapCertificate(ctx context.Context, certificateMap, sni string) (*tls.Certificate, error) {
	n, err := parseName(kMap, certificateMap)
	if err != nil {
		return nil, err
	}
	mapName := n.name(kMap)
	key := mapName + "|" + strings.ToLower(strings.TrimSuffix(sni, "."))
	s.cache.mu.Lock()
	s.cache.sync()
	if c, ok := s.cache.maps[key]; ok {
		s.cache.mu.Unlock()
		return c, nil
	}
	gen := s.cache.at
	s.cache.mu.Unlock()

	var mapObj obj
	var entries []obj
	_ = s.env.Store.View(func(tx store.Tx) error {
		mapObj = load(tx, mapName)
		entries = scan(tx, kEntry, resName{Project: n.Project, Location: n.Location, Parent: n.ID})
		return nil
	})
	if mapObj == nil {
		return nil, notFound(mapName)
	}
	var sel *tls.Certificate
	for _, e := range selectEntry(entries, sni) {
		for _, c := range strs(e["certificates"]) {
			if cert, err := s.Certificate(ctx, c); err == nil {
				sel = cert
				break
			}
		}
		if sel != nil {
			break
		}
	}
	s.cache.mu.Lock()
	if s.cache.at == gen {
		s.cache.maps[key] = sel
	}
	s.cache.mu.Unlock()
	return sel, nil
}

// Certificate implements emu.CertManager.
func (s *Service) Certificate(ctx context.Context, name string) (*tls.Certificate, error) {
	n, err := parseName(kCert, name)
	if err != nil {
		return nil, err
	}
	name = n.name(kCert)
	s.cache.mu.Lock()
	s.cache.sync()
	if c := s.cache.certs[name]; c != nil {
		s.cache.mu.Unlock()
		return c, nil
	}
	gen := s.cache.at
	s.cache.mu.Unlock()

	var o obj
	var rec keyRecord
	var recErr error
	_ = s.env.Store.View(func(tx store.Tx) error {
		o = load(tx, name)
		recErr = store.GetJSON(tx, nsKeys, name, &rec)
		return nil
	})
	if o == nil {
		return nil, notFound(name)
	}
	if !certActive(o) {
		st, _ := getPath(o, "managed.state")
		return nil, apierr.FailedPrecondition("Certificate %s is not ACTIVE (state %v).", name, st)
	}
	if recErr != nil {
		return nil, apierr.FailedPrecondition("Certificate %s has no key material.", name)
	}
	cert, err := tls.X509KeyPair([]byte(rec.Chain), []byte(rec.Key))
	if err != nil {
		return nil, apierr.Internal("parse certificate %s: %v", name, err)
	}
	s.cache.mu.Lock()
	if s.cache.at == gen {
		s.cache.certs[name] = &cert
	}
	s.cache.mu.Unlock()
	return &cert, nil
}

// BackendAuthentication implements emu.CertManager. roots holds the trust
// config's anchors (plus intermediates that chain to them and allowlisted
// certificates); with wellKnownRoots PUBLIC_ROOTS it also holds the system
// roots and the instance CA, which plays the role of the public CAs for
// emulated workloads. NONE without a trust config yields an empty pool.
func (s *Service) BackendAuthentication(ctx context.Context, name string) (*tls.Certificate, *x509.CertPool, error) {
	o, _, err := s.loadKind(kBAC, name)
	if err != nil {
		return nil, nil, err
	}
	b, err := as[nsv1.BackendAuthenticationConfig](o)
	if err != nil {
		return nil, nil, err
	}
	var client *tls.Certificate
	if b.ClientCertificate != "" {
		if client, err = s.Certificate(ctx, b.ClientCertificate); err != nil {
			return nil, nil, err
		}
	}
	roots := x509.NewCertPool()
	if b.WellKnownRoots == "PUBLIC_ROOTS" {
		if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
			roots = sys
		}
		if s.env.CA != nil {
			roots.AddCert(s.env.CA.Certificate())
		}
	}
	if b.TrustConfig != "" {
		t, _, err := s.loadKind(kTrust, b.TrustConfig)
		if err != nil {
			return nil, nil, err
		}
		addPool(roots, t)
	}
	return client, roots, nil
}

// ServerTLSPolicy implements emu.CertManager. A policy without mtlsPolicy
// returns a nil pool and mode "" (do not request client certificates).
func (s *Service) ServerTLSPolicy(ctx context.Context, name string) (*x509.CertPool, string, error) {
	o, _, err := s.loadKind(kServerTLS, name)
	if err != nil {
		return nil, "", err
	}
	p, err := as[nsv1.ServerTlsPolicy](o)
	if err != nil {
		return nil, "", err
	}
	if p.MtlsPolicy == nil {
		return nil, "", nil
	}
	mode := p.MtlsPolicy.ClientValidationMode
	if mode == "" {
		mode = "ALLOW_INVALID_OR_MISSING_CLIENT_CERT"
	}
	roots := x509.NewCertPool()
	if tc := p.MtlsPolicy.ClientValidationTrustConfig; tc != "" {
		t, _, err := s.loadKind(kTrust, tc)
		if err != nil {
			return nil, "", err
		}
		addPool(roots, t)
	}
	return roots, mode, nil
}

// addPool adds a trust config's effective roots to pool.
func addPool(pool *x509.CertPool, trust obj) {
	for _, c := range trustCerts(trust) {
		pool.AddCert(c)
	}
}
