// Package iam emulates Identity and Access Management (SRS 5.1): service
// accounts and keys, predefined and custom roles, IAM policies on projects
// and resources with a shared policy evaluator, OAuth2 token issuance, the
// IAM Credentials API and a GCE-compatible metadata server. It also
// provides the emulator's authenticator (FR-CORE-050..052).
package iam

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

	credentialspb "cloud.google.com/go/iam/credentials/apiv1/credentialspb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Service is the iam service module.
type Service struct {
	env *emu.Env
	cat *catalogue

	signer *signingKey // emulator OIDC signing key

	mu       sync.Mutex
	started  bool
	metaL    net.Listener
	metaSrv  *http.Server
	tokCache map[string]cachedToken // metadata token cache, by email
	meta     metadataSettings
}

// Compile-time checks of the cross-service contracts.
var (
	_ emu.ServiceAccountKeys  = (*Service)(nil)
	_ emu.IAMPolicyStore      = (*Service)(nil)
	_ emu.IAMResourceParents  = (*Service)(nil)
	_ emu.IAMPermissionTester = (*Service)(nil)
	_ emu.PolicyEvaluator     = (*Service)(nil)
	_ emu.Authenticator       = (*Service)(nil)
	_ emu.Seeder              = (*Service)(nil)
	_ emu.EnvVarer            = (*Service)(nil)
	_ emu.Resetter            = (*Service)(nil)
)

// New returns the service.
func New(env *emu.Env) emu.Service {
	return &Service{env: env, cat: builtin(), tokCache: map[string]cachedToken{}}
}

func (s *Service) Name() string { return "iam" }

// Register mounts iam, cloudresourcemanager, oauth2 and iamcredentials and
// installs the policy evaluator into the authorizer.
func (s *Service) Register(r emu.Router) error {
	dir, err := s.env.ServiceDir("iam")
	if err != nil {
		return err
	}
	if s.signer, err = loadSigningKey(dir, s.env.Clock.Now()); err != nil {
		return err
	}
	if pa, ok := s.env.Auth.(*emu.PolicyAuthorizer); ok {
		pa.Install(s, s)
	}
	r.Mount("iam", []string{"iam.googleapis.com"}, http.HandlerFunc(s.serveIAM))
	r.Mount("cloudresourcemanager", []string{"cloudresourcemanager.googleapis.com"}, http.HandlerFunc(s.serveCRM))
	r.Mount("oauth2", []string{"oauth2.googleapis.com", "accounts.google.com"}, http.HandlerFunc(s.serveOAuth2))
	r.Mount("iamcredentials", []string{"iamcredentials.googleapis.com"}, http.HandlerFunc(s.serveCredentials))
	r.Mount("sts", []string{"sts.googleapis.com"}, http.HandlerFunc(s.serveSTS))
	r.Handle("/service_accounts/v1/metadata/", http.HandlerFunc(s.serveX509))
	r.Handle("GET /.well-known/openid-configuration", http.HandlerFunc(s.serveDiscovery))
	credentialspb.RegisterIAMCredentialsServer(r.GRPC(), &credentialsServer{s: s})
	return nil
}

// Start opens the metadata server listener (FR-IAM-008).
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.meta.Project == "" {
		s.meta = s.metadataDefaults()
	}
	s.mu.Unlock()
	l, err := s.env.Listen("metadata")
	if err != nil {
		return err
	}
	srv := gateway.NewServer(s.env.Middleware("iam", s.MetadataHandler(MetadataIdentity{})), s.env.Log)
	s.mu.Lock()
	s.metaL, s.metaSrv, s.started = l, srv, true
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.env.Log.Error("metadata server stopped", "err", err)
		}
	}()
	return nil
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.metaSrv
	s.metaSrv, s.started = nil, false
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *Service) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return errors.New("metadata server not started")
	}
	return nil
}

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	out := store.Counts(s.env.Store, map[string]string{
		"serviceAccounts": nsAccounts, "serviceAccountKeys": nsKeys, "roles": nsRoles,
		"workloadIdentityPools": nsPools, "workloadIdentityPoolProviders": nsProviders,
	})
	out["projects"] = len(s.knownProjects())
	return out
}

// Reset drops in-memory caches; persistent state lives in the store, which
// the instance wipes.
func (s *Service) Reset(ctx context.Context) error {
	s.mu.Lock()
	s.tokCache = map[string]cachedToken{}
	s.mu.Unlock()
	return nil
}
