// Package dns emulates Cloud DNS (SRS 5.2): the dns.googleapis.com v1 REST
// control plane (managed zones, record sets, changes; FR-DNS-001/002) and an
// authoritative DNS data plane on UDP and TCP that answers from the stored
// zones and forwards other names to the host resolver (FR-DNS-003/004).
package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Service is the Cloud DNS service module.
type Service struct {
	env *emu.Env

	genSeq  atomic.Uint64
	genBase string

	ixMu    sync.Mutex
	ix      *index
	ixGen   string
	ixDirty bool

	upstreams []string

	// mapper rewrites A record addresses (SetAddressMapper).
	mapper atomic.Pointer[func(net.IP) net.IP]

	mu      sync.Mutex
	udp     *mdns.Server
	tcp     *mdns.Server
	addr    string
	started bool
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	return &Service{env: env, genBase: strconv.FormatInt(time.Now().UnixNano(), 36), ixDirty: true}
}

func (s *Service) Name() string { return "dns" }

// Register mounts the REST API at /dns/v1/ (root, as the Go client and
// gcloud address it), under the /dns/ gateway prefix and in host mode.
func (s *Service) Register(r emu.Router) error {
	h := s.handler()
	r.Handle("/dns/v1/", h)
	r.Mount("dns", []string{"dns.googleapis.com"}, h)
	return nil
}

// Start opens the DNS data plane: TCP via env.Listen("dns") and UDP on the
// same port number.
func (s *Service) Start(ctx context.Context) error {
	s.upstreams = s.hostResolvers()
	var (
		tl  net.Listener
		pc  net.PacketConn
		err error
	)
	for attempt := 0; attempt < 20; attempt++ {
		tl, err = s.env.Listen("dns")
		if err != nil {
			return fmt.Errorf("dns tcp listen: %w", err)
		}
		_, port, _ := net.SplitHostPort(tl.Addr().String())
		pc, err = listenUDP(ctx, s.env.Config.Bind, port)
		if err == nil {
			break
		}
		_ = tl.Close()
		if s.env.Config.Port("dns") != 0 {
			return fmt.Errorf("dns udp listen: %w", err)
		}
	}
	if err != nil {
		return fmt.Errorf("dns udp listen: %w", err)
	}
	h := mdns.HandlerFunc(s.serveDNS)
	var wg sync.WaitGroup
	wg.Add(2)
	tcp := &mdns.Server{Listener: tl, Handler: h, Net: "tcp", NotifyStartedFunc: wg.Done}
	udp := &mdns.Server{PacketConn: pc, Handler: h, Net: "udp", NotifyStartedFunc: wg.Done}
	for _, srv := range []*mdns.Server{tcp, udp} {
		go func(srv *mdns.Server) {
			if err := srv.ActivateAndServe(); err != nil {
				s.env.Log.Debug("dns server stopped", "net", srv.Net, "err", err)
			}
		}(srv)
	}
	wg.Wait()
	s.mu.Lock()
	s.tcp, s.udp, s.addr, s.started = tcp, udp, tl.Addr().String(), true
	s.mu.Unlock()
	return nil
}

// Stop shuts the data plane down.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	tcp, udp := s.tcp, s.udp
	s.tcp, s.udp, s.started = nil, nil, false
	s.mu.Unlock()
	var errs []error
	for _, srv := range []*mdns.Server{tcp, udp} {
		if srv != nil {
			if err := srv.ShutdownContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Ready reports whether the data plane is listening.
func (s *Service) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return errors.New("dns server not started")
	}
	return nil
}

// Reset drops the in-memory query index; it is rebuilt from the store on
// the next query.
func (s *Service) Reset(context.Context) error {
	s.ixMu.Lock()
	s.ix, s.ixDirty = nil, true
	s.ixMu.Unlock()
	return nil
}

// Addr returns the data plane's host:port (UDP and TCP).
func (s *Service) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// EnvVars implements emu.EnvVarer. gcloud's dns client has base URL
// https://dns.googleapis.com/dns/v1/, so the override includes /dns/v1/.
func (s *Service) EnvVars(gateway string, endpoints map[string]string) map[string]string {
	out := map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_DNS": "http://" + gateway + "/dns/v1/",
	}
	if a := endpoints["dns"]; a != "" {
		out["GCPEMU_DNS"] = a
	}
	return out
}

// bumpGen records a new store generation inside a write transaction so the
// query index is rebuilt after the commit (also after snapshot restores).
func (s *Service) bumpGen(tx store.Tx) error {
	g := s.genBase + "-" + strconv.FormatUint(s.genSeq.Add(1), 36)
	return tx.Put(nsMeta, "gen", []byte(g))
}

// update runs fn in a write transaction and bumps the generation.
func (s *Service) update(fn func(tx store.Tx) error) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return s.bumpGen(tx)
	})
}
