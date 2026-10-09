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
	"sync"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/genproto/googleapis/cloud/location"

	"github.com/linuxuser586/gcpemu/internal/emu"
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

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var (
	_ emu.Service         = (*Service)(nil)
	_ emu.ClockObserver   = (*Service)(nil)
	_ emu.ResourceCounter = (*Service)(nil)
	_ emu.Seeder          = (*Service)(nil)
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

// Start runs the sweep of time-driven changes.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	bg, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		s.loop(bg, sweepInterval)
	}(s.done)
	return nil
}

// Stop stops the sweep.
func (s *Service) Stop(ctx context.Context) error {
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
