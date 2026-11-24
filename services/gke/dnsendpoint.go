package gke

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Serving the DNS endpoints. The emulated DNS answers a host query for a
// cluster's DNS endpoint (under gke.goog, which `gcpemu dns install
// --suffix gke.goog` routes to it) with one Loopback alias, taken from the
// Loopback alias pool forwarding rules, Cloud SQL instances and Kubernetes Services share
// on the first query. There the emulator listens on port 443, as clients
// of a DNS endpoint dial it, and terminates TLS with a certificate from
// the Instance CA for the name the client asked for, which Host trust
// verifies: gcloud's kubeconfig for a DNS endpoint carries no CA. On macOS,
// where an unprivileged process binds port 443 only on the wildcard
// address, it listens there, serving connections to loopback only. Each
// request needs a caller with container.clusters.connect on the cluster,
// and a DNS endpoint that allows external traffic, as host clients are
// outside Google Cloud; it then runs on the API server as the caller, as
// through the Connect gateway. When the endpoint cannot be served (no
// aliases, a Containerized Instance, port 443 refused), the query gets
// SERVFAIL and the Admin API says why.

// dnsEPLoopName names the DNS endpoint listener's Loopback alias among
// loopCandidates names.
const dnsEPLoopName = "dns-endpoint"

// dnsEndpointPort is the port DNS endpoints are served on; tests replace
// it.
var dnsEndpointPort = 443

// dnsEPServer is the listener serving every cluster's DNS endpoint.
type dnsEPServer struct {
	ip   string
	addr string
	srv  *http.Server
}

// answerDNSEndpoint answers a host query for name, lower case and fully
// qualified, under gke.goog.
func (s *Service) answerDNSEndpoint(ctx context.Context, name string, qtype uint16) ([]mdns.RR, int) {
	if s.clusterByDNSEndpoint(strings.TrimSuffix(name, ".")) == nil {
		return nil, mdns.RcodeNameError
	}
	if qtype != mdns.TypeA && qtype != mdns.TypeANY {
		return nil, mdns.RcodeSuccess
	}
	ip, err := s.dnsEndpointIP(ctx)
	if err != nil {
		return nil, mdns.RcodeServerFailure
	}
	hdr := mdns.RR_Header{Name: name, Class: mdns.ClassINET, Ttl: hostSvcTTL, Rrtype: mdns.TypeA}
	return []mdns.RR{&mdns.A{Hdr: hdr, A: net.ParseIP(ip)}}, mdns.RcodeSuccess
}

// clusterByDNSEndpoint returns the cluster whose DNS endpoint is host,
// nil when none has it.
func (s *Service) clusterByDNSEndpoint(host string) *clusterRecord {
	host = strings.ToLower(host)
	if !strings.HasSuffix(host, dnsEndpointSuffix) {
		return nil
	}
	var found *clusterRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, rec := range listClusters(tx, "") {
			if rec.cluster().GetControlPlaneEndpointsConfig().GetDnsEndpointConfig().GetEndpoint() == host {
				found = rec
				return nil
			}
		}
		return nil
	})
	return found
}

// dnsEndpointIP returns the DNS endpoint listener's Loopback alias,
// serving it first if it is not. A reason it cannot be served is logged
// when it changes.
func (s *Service) dnsEndpointIP(ctx context.Context) (string, error) {
	s.dnsEPMu.Lock()
	defer s.dnsEPMu.Unlock()
	if s.dnsEP != nil {
		return s.dnsEP.ip, nil
	}
	ep, err := s.serveDNSEndpoint(ctx)
	prev := s.dnsEPFail
	s.dnsEPFail = err
	if err != nil {
		if prev == nil || prev.Error() != err.Error() {
			s.env.Log.Warn("gke: cannot serve DNS endpoints; host queries for them get SERVFAIL", "err", err)
		}
		return "", err
	}
	s.dnsEP = ep
	return ep.ip, nil
}

// serveDNSEndpoint starts the DNS endpoint listener on the first free
// Loopback alias. Callers hold dnsEPMu.
func (s *Service) serveDNSEndpoint(ctx context.Context) (*dnsEPServer, error) {
	switch {
	case s.env.CA == nil:
		return nil, errors.New("the Instance has no Instance CA to serve DNS endpoints with")
	case s.env.Containerized(ctx):
		return nil, errors.New("a Containerized Instance has no host loopback to serve DNS endpoints on")
	case !hostAliases():
		return nil, fmt.Errorf("the host has no loopback aliases; run `%s`", loopback.InstallHint)
	}
	cands := s.loopCandidates(dnsEPLoopName, "")
	if len(cands) == 0 {
		return nil, errPoolExhausted
	}
	port := strconv.Itoa(dnsEndpointPort)
	for _, ip := range cands {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, port))
		switch refused := loopback.Refused(err); {
		case err == nil:
		case refused == loopback.PrivilegedWildcard:
			wln, werr := net.Listen("tcp", net.JoinHostPort("0.0.0.0", port))
			if werr != nil {
				return nil, fmt.Errorf("port %s is privileged on this host, and another process holds it on the wildcard address: %w", port, werr)
			}
			ln = &loopback.LocalFilter{Listener: wln, Allow: loopback.IsLoopback}
		case refused == loopback.Privileged:
			return nil, fmt.Errorf("the host refuses an unprivileged process port %s; allow it with `sudo sysctl net.ipv4.ip_unprivileged_port_start=%s`: %w", port, port, err)
		case loopback.AddrInUse(err):
			continue
		default:
			return nil, err
		}
		_, p, _ := net.SplitHostPort(ln.Addr().String())
		ep := &dnsEPServer{ip: ip, addr: net.JoinHostPort(ip, p)}
		ep.srv = &http.Server{
			Handler:           http.HandlerFunc(s.serveDNSEndpointRequest),
			TLSConfig:         &tls.Config{GetCertificate: s.dnsEndpointCert, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}},
			ReadHeaderTimeout: 30 * time.Second,
			ErrorLog:          slog.NewLogLogger(s.env.Log.Handler(), slog.LevelDebug),
		}
		go func() { _ = ep.srv.ServeTLS(ln, "", "") }()
		s.loopMu.Lock()
		s.loops[dnsEPLoopName] = ip
		s.loopMu.Unlock()
		return ep, nil
	}
	return nil, errPoolExhausted
}

// closeDNSEndpoint stops the DNS endpoint listener and frees its Loopback
// alias.
func (s *Service) closeDNSEndpoint() {
	s.dnsEPMu.Lock()
	defer s.dnsEPMu.Unlock()
	if s.dnsEP == nil {
		return
	}
	_ = s.dnsEP.srv.Close()
	s.dnsEP = nil
	s.loopMu.Lock()
	delete(s.loops, dnsEPLoopName)
	s.loopMu.Unlock()
}

// dnsEndpointCert issues the certificate for the DNS endpoint a client
// names, refusing names no cluster has.
func (s *Service) dnsEndpointCert(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(hello.ServerName)
	if s.clusterByDNSEndpoint(name) == nil {
		return nil, fmt.Errorf("%q is no cluster's DNS endpoint", hello.ServerName)
	}
	return s.env.CA.ServerCert(name)
}

// serveDNSEndpointRequest passes one request to a DNS endpoint's API
// server, as its caller.
func (s *Service) serveDNSEndpointRequest(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	rec := s.clusterByDNSEndpoint(host)
	if rec == nil {
		apierr.Write(w, apierr.NotFound("%q is no cluster's DNS endpoint.", host))
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		apierr.Write(w, apierr.Unauthenticated("The DNS endpoint of cluster %s needs a Google access token; use the kubeconfig `gcloud container clusters get-credentials --dns-endpoint` writes.", rec.Int.Name))
		return
	}
	p, err := s.env.Auth.Authenticate(r.Context(), token)
	if err != nil {
		if errors.Is(err, emu.ErrUnknownToken) {
			err = apierr.Unauthenticated("The access token is not one this Instance issued.")
		}
		apierr.Write(w, err)
		return
	}
	ctx := emu.WithPrincipal(r.Context(), p)
	cl := ref{Project: rec.Int.Project, Location: rec.Int.Location, Cluster: rec.Int.Name}
	if err := s.env.Auth.Check(ctx, "container.clusters.connect", cl.clusterResource()); err != nil {
		apierr.Write(w, err)
		return
	}
	if !rec.cluster().GetControlPlaneEndpointsConfig().GetDnsEndpointConfig().GetAllowExternalTraffic() {
		apierr.Write(w, apierr.PermissionDenied("The DNS endpoint of cluster %s does not allow external traffic, and host clients are outside Google Cloud; set dnsEndpointConfig.allowExternalTraffic.", rec.Int.Name))
		return
	}
	s.proxyKube(w, r.WithContext(ctx), rec, r.URL.Path)
}

// dnsEndpointJSON is the DNS endpoint listener in the Admin API: where
// it is served, or why it is not.
type dnsEndpointJSON struct {
	Address string `json:"address,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// dnsEndpointStatus is the DNS endpoint listener's state.
func (s *Service) dnsEndpointStatus() dnsEndpointJSON {
	s.dnsEPMu.Lock()
	defer s.dnsEPMu.Unlock()
	switch {
	case s.dnsEP != nil:
		return dnsEndpointJSON{Address: s.dnsEP.addr}
	case s.dnsEPFail != nil:
		return dnsEndpointJSON{Reason: s.dnsEPFail.Error()}
	}
	return dnsEndpointJSON{}
}
