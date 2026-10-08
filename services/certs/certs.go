// Package certs emulates Certificate Manager
// (certificatemanager.googleapis.com v1) and the Network Security API
// (networksecurity.googleapis.com v1: backend authentication configs,
// server and client TLS policies) and provides emu.CertManager, the view
// of those resources the load balancer data plane consumes (FR-LB-004,
// FR-LB-006, FR-LB-009).
//
// Both APIs are served over gRPC (the official Go clients) and REST
// (gcloud, OpenTofu) on the gateway; mutations are google.longrunning
// operations (internal/lro). Resources are stored in their REST JSON form,
// which tracks the current googleapis protos (decision 11.3) — e.g.
// TrustConfig.allowlistedCertificates and certificate scope CLIENT_AUTH,
// which the published Go gRPC stubs do not carry yet; over gRPC such
// fields are omitted.
//
// Managed certificates are issued by the instance CA (env.CA, Section 7.4)
// once their domains are authorized (certificates.go).
package certs

import (
	"context"
	"net/http"
	"sync"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/genproto/googleapis/cloud/location"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/lro"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Service is the certs service module.
type Service struct {
	env   *emu.Env
	ops   *lro.Manager
	cache tlsCache
	kickC chan struct{}

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	return &Service{env: env, ops: lro.NewManager(env, "certs"), kickC: make(chan struct{}, 1)}
}

func (s *Service) Name() string { return "certs" }

// Register installs the gRPC services, the REST surfaces and the CA admin
// endpoint on the gateway.
func (s *Service) Register(r emu.Router) error {
	s.ops.Register(r)
	g := r.GRPC()
	certificatemanagerpb.RegisterCertificateManagerServer(g, &cmServer{s: s})
	networksecuritypb.RegisterNetworkSecurityServer(g, &nsServer{s: s})
	if _, taken := g.GetServiceInfo()["google.cloud.location.Locations"]; !taken {
		location.RegisterLocationsServer(g, &locationsServer{s: s, api: "certificatemanager"})
	}
	r.Mount("certificatemanager", []string{cmHost}, s.restHandler("certificatemanager"))
	r.Mount("networksecurity", []string{nsHost}, s.restHandler("networksecurity"))
	r.Handle("POST /_emu/v1/ca/rotate", http.HandlerFunc(s.serveRotate))
	return nil
}

// Start runs the managed-certificate provisioner.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	bg, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	s.invalidate()
	go func(done chan struct{}) {
		defer close(done)
		s.reconcileLoop(bg)
	}(s.done)
	return nil
}

// Stop stops the provisioner and pending operations.
func (s *Service) Stop(ctx context.Context) error {
	s.ops.Close()
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return nil
}

func (s *Service) Ready() error { return nil }

// Operations implements emu.OperationLister.
func (s *Service) Operations() []emu.OperationInfo { return s.ops.Operations() }

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	out := map[string]int{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, k := range allKinds {
			out[k.coll] = 0
		}
		tx.Scan(nsRes, "projects/", func(key string, _ []byte) bool {
			for _, k := range allKinds {
				if _, err := parseName(k, key); err == nil {
					out[k.coll]++
					break
				}
			}
			return true
		})
		return nil
	})
	return out
}

// Reset drops cached certificates; the store is reset by the instance.
func (s *Service) Reset(ctx context.Context) error {
	s.invalidate()
	return nil
}

// ClockAdvanced re-evaluates managed certificates.
func (s *Service) ClockAdvanced(ctx context.Context) error {
	s.invalidate()
	s.kick()
	return nil
}

// EnvVars implements emu.EnvVarer (FR-CORE-004): gcloud endpoint
// overrides for `gcloud certificate-manager` and `gcloud network-security`.
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	return map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_CERTIFICATEMANAGER": "http://" + gw + "/certificatemanager/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_NETWORKSECURITY":    "http://" + gw + "/networksecurity/",
	}
}
