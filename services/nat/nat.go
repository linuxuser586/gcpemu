// Package nat is the Cloud NAT service module (SRS 5.10). Cloud NAT's
// control plane is part of compute.googleapis.com (routers and their
// nats[], FR-NAT-001) and is served by the compute module, as is the
// egress gateway data plane (FR-NAT-002..005), which compute's emu.VPC
// starts when workloads ask for their egress gateway. This module exists
// so `--services nat` selects Cloud NAT (pulling in compute through
// config.Dependencies) and so readiness reflects whether a container
// runtime is available for the data plane.
package nat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Service is the Cloud NAT service module.
type Service struct {
	env *emu.Env

	mu      sync.Mutex
	checked time.Time
	err     error
}

// New returns the service.
func New(env *emu.Env) emu.Service { return &Service{env: env} }

func (s *Service) Name() string                { return "nat" }
func (s *Service) Register(r emu.Router) error { return nil }

// Start probes the runtime in the background; instance start never fails
// because of it (the control plane works without a runtime).
func (s *Service) Start(ctx context.Context) error {
	go func() {
		pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.probe(pctx)
	}()
	return nil
}

func (s *Service) Stop(ctx context.Context) error { return nil }

// Ready reports whether the egress gateway data plane can run: compute
// must be running and a container runtime reachable. The result is
// cached for a few seconds.
func (s *Service) Ready() error {
	if _, ok := s.env.Lookup("compute"); !ok {
		return fmt.Errorf("nat: the compute service (Cloud Router / NAT control plane) is not running")
	}
	s.mu.Lock()
	fresh := !s.checked.IsZero() && time.Since(s.checked) < 5*time.Second
	err := s.err
	s.mu.Unlock()
	if fresh {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.probe(ctx)
}

func (s *Service) probe(ctx context.Context) error {
	var err error
	if s.env.Containers == nil {
		err = fmt.Errorf("nat: no container runtime support in this build")
	} else if _, rerr := s.env.Containers.Runtime(ctx); rerr != nil {
		err = fmt.Errorf("nat: egress gateway needs a container runtime: %w", rerr)
	}
	s.mu.Lock()
	s.checked, s.err = time.Now(), err
	s.mu.Unlock()
	return err
}
