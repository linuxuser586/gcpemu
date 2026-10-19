// Package secrets emulates Secret Manager (secretmanager.googleapis.com
// v1) over gRPC and REST on the gateway: global and regional secrets,
// versions and their payloads, version aliases, expiry, delayed version
// destruction, rotation notifications to Pub/Sub topics, managed rotation
// of Cloud SQL credentials, and secret IAM policies. Time-driven behaviour
// follows the Emulator clock (lifecycle.go). Replication policies and
// customer-managed encryption are Recorded.
package secrets

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/genproto/googleapis/cloud/location"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/inflight"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// sweepInterval is how often time-driven changes are applied without a
// clock advance.
const sweepInterval = time.Second

// Service is the secrets service module.
type Service struct {
	env   *emu.Env
	api   *api
	kickC chan struct{}

	sweepMu sync.Mutex // one sweep at a time, side effects included

	// loops runs the sweep loop; Reset restarts it (#307).
	loops inflight.Loops
}

var (
	_ emu.Service         = (*Service)(nil)
	_ emu.ClockObserver   = (*Service)(nil)
	_ emu.Resetter        = (*Service)(nil)
	_ emu.ResetFinisher   = (*Service)(nil)
	_ emu.ResourceCounter = (*Service)(nil)
	_ emu.Seeder          = (*Service)(nil)
	_ emu.SeedExporter    = (*Service)(nil)
	_ emu.EnvVarer        = (*Service)(nil)
)

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{env: env, kickC: make(chan struct{}, 1)}
	s.api = &api{s: s}
	return s
}

func (s *Service) Name() string { return "secrets" }

// Register installs the gRPC service and the REST surface, on the global
// and every regional hostname.
func (s *Service) Register(r emu.Router) error {
	g := r.GRPC()
	secretmanagerpb.RegisterSecretManagerServiceServer(g, s.api)
	if _, taken := g.GetServiceInfo()["google.cloud.location.Locations"]; !taken {
		location.RegisterLocationsServer(g, &locationsServer{s: s})
	}
	hosts := []string{apiHost}
	for _, reg := range locations.Regions() {
		hosts = append(hosts, regionalHost(reg.Name))
	}
	r.Mount("secretmanager", hosts, s.restHandler())
	return nil
}

// Start runs the sweep of time-driven changes, the first one at once.
func (s *Service) Start(ctx context.Context) error {
	s.loops.Start(func(ctx context.Context) { s.loop(ctx, sweepInterval) })
	s.kick()
	return nil
}

// Stop stops the sweep.
func (s *Service) Stop(ctx context.Context) error {
	_ = s.loops.Stop(ctx)
	return nil
}

// Reset stops a sweep in flight, waiting for it (bounded by ctx), so none
// of its changes, notifications or rotations from the state before the
// Reset lands after it (#307); the store is reset by the instance, and the
// sweep runs again from ResetDone.
func (s *Service) Reset(ctx context.Context) error {
	select {
	case <-s.kickC:
	default:
	}
	if err := s.loops.Reset(ctx); err != nil {
		return fmt.Errorf("secrets: waiting for the sweep: %w", err)
	}
	return nil
}

// ResetDone runs the sweep again once the store is reset too.
func (s *Service) ResetDone() { s.loops.Resume() }

func (s *Service) Ready() error { return nil }

// ClockAdvanced applies what became due before returning.
func (s *Service) ClockAdvanced(ctx context.Context) error {
	s.sweep(ctx)
	return nil
}

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	out := map[string]int{"secrets": 0, "versions": 0}
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsSecrets, "", func(string, []byte) bool { out["secrets"]++; return true })
		tx.Scan(nsVersions, "", func(string, []byte) bool { out["versions"]++; return true })
		return nil
	})
	return out
}

// EnvVars implements emu.EnvVarer (FR-CORE-004): the gcloud endpoint
// override for `gcloud secrets`.
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	return map[string]string{"CLOUDSDK_API_ENDPOINT_OVERRIDES_SECRETMANAGER": "http://" + gw + "/secretmanager/"}
}

var _ emu.SecretAccessor = (*Service)(nil)

// AccessSecretVersion implements emu.SecretAccessor.
func (s *Service) AccessSecretVersion(ctx context.Context, name string) ([]byte, string, error) {
	resp, err := s.api.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return nil, "", err
	}
	return resp.GetPayload().GetData(), resp.GetName(), nil
}
