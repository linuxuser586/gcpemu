// Package ar emulates Artifact Registry (SRS 5.3): the artifactregistry v1
// control plane over gRPC and REST on the gateway, and an OCI Distribution
// v1.1 registry on its own port (default 5000).
//
// # Control plane
//
// Repositories (FR-AR-001) of format DOCKER can be created, read, listed,
// patched and deleted in every Artifact Registry location; create and
// delete return google.longrunning operations (internal/lro). Besides
// STANDARD_REPOSITORY, the REMOTE_REPOSITORY (Docker Hub or a custom
// registry, pulled through a cache) and VIRTUAL_REPOSITORY (members by
// upstream policy priority) modes are served (FR-AR-006, remote.go).
// Other formats return UNIMPLEMENTED. Packages,
// versions, tags and dockerImages (FR-AR-004) are views derived from the
// registry contents. REST is served by transcoding the google.api.http
// annotations of the generated descriptors onto the same gRPC
// implementation, so both surfaces behave identically.
//
// # Registry image names
//
// The registry accepts three spellings of an image name, so that docker,
// crane, ko and containerd work with nothing but a host change:
//
//   - Host mode: the request Host is "LOCATION-docker.pkg.dev" and the path
//     is /v2/PROJECT/REPO/IMAGE[/...]. The location comes from the host.
//   - Location in the path: /v2/LOCATION-docker.pkg.dev/PROJECT/REPO/IMAGE,
//     e.g. "localhost:5000/us-docker.pkg.dev/p/r/app". This keeps the real
//     image reference intact after a registry-host prefix and is
//     unambiguous.
//   - Plain port: /v2/PROJECT/REPO/IMAGE, e.g. "localhost:5000/p/r/app". The
//     repository is found by project and repository ID across locations; if
//     the same ID exists in several locations the request fails with
//     NAME_INVALID naming them, and the caller must use one of the forms
//     above.
//
// In every form the OCI repository name used for blob links, uploads,
// mounts and tags is PROJECT/REPO/IMAGE within the resolved repository, and
// the AR API reports image URIs as LOCATION-docker.pkg.dev/PROJECT/REPO/IMAGE.
//
// # Public registry mirror (FR-GKE-006)
//
// The registry also mirrors public registries for GKE nodes: containerd
// mirror requests (?ns=docker.io) and the path form
// REGISTRY/docker.io/library/busybox are pulled through a persistent,
// digest-verified cache that keeps working offline (mirror.go,
// pullthrough.go, upstream.go). RegistriesYAML renders the k3s node
// configuration (registries.go).
//
// # Registry auth (FR-AR-003)
//
// Clients are challenged with "WWW-Authenticate: Bearer realm=<host>/v2/token"
// like the real service. The token endpoint and every registry request
// accept Basic credentials "oauth2accesstoken:<emulator access token>" (also
// the "_token" and "_dcgcloud_token" user names) and "_json_key:<service
// account key JSON>" (verified against the iam service's public keys), and
// Bearer emulator tokens. Anonymous access is allowed when IAM is off or in
// audit mode; in enforce mode credentials are required and
// artifactregistry.repositories.downloadArtifacts / uploadArtifacts /
// deleteArtifacts are checked on the repository.
package ar

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/genproto/googleapis/cloud/location"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/lro"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Service is the Artifact Registry service module.
type Service struct {
	env   *emu.Env
	ops   *lro.Manager
	blobs *blobStore
	api   *api

	mu      sync.Mutex
	srv     *http.Server
	conns   connTracker
	ready   bool
	uploads map[string]*upload

	// Pull-through cache (pullthrough.go): upstream client, base URL
	// overrides, tag TTL, coalesced manifest fetches, in-flight blob
	// downloads and the context they run under.
	upstream     *upstreamClient
	upstreamURLs map[string]string
	tagTTL       time.Duration
	flights      flightGroup
	fetches      map[string]*blobFetch
	bgCtx        context.Context
	bgCancel     context.CancelFunc
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{
		env: env, ops: lro.NewManager(env, "ar"), uploads: map[string]*upload{},
		upstreamURLs: map[string]string{}, tagTTL: defaultTagTTL, fetches: map[string]*blobFetch{},
	}
	s.upstream = newUpstreamClient(env.Clock.Now)
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.api = &api{s: s}
	return s
}

func (s *Service) Name() string { return "ar" }

// Register installs the artifactregistry gRPC and REST surfaces, the
// Locations mixin and google.longrunning Operations on the gateway.
func (s *Service) Register(r emu.Router) error {
	dir, err := s.env.ServiceDir("ar")
	if err != nil {
		return err
	}
	if s.blobs, err = newBlobStore(dir); err != nil {
		return err
	}
	s.ops.Register(r)
	g := r.GRPC()
	artifactregistrypb.RegisterArtifactRegistryServer(g, s.api)
	if _, taken := g.GetServiceInfo()["google.cloud.location.Locations"]; !taken {
		location.RegisterLocationsServer(g, &locationsServer{s: s})
	}
	r.Mount("artifactregistry", []string{apiHost}, s.restHandler())
	return nil
}

// Start opens the registry listener (FR-AR-002).
func (s *Service) Start(ctx context.Context) error {
	l, err := s.env.Listen("ar")
	if err != nil {
		return err
	}
	srv := gateway.NewServer(s.withRegistryAuth(s.env.Middleware("ar", http.HandlerFunc(s.serveRegistry))), s.env.Log)
	srv.ConnState = s.conns.track
	s.mu.Lock()
	if s.bgCtx.Err() != nil {
		s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	}
	s.srv, s.ready = srv, true
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.env.Log.Error("ar registry stopped", "err", err)
		}
	}()
	return nil
}

// Stop shuts the registry down and stops pending operations.
func (s *Service) Stop(ctx context.Context) error {
	s.ops.Close()
	s.mu.Lock()
	srv := s.srv
	s.srv, s.ready = nil, false
	s.bgCancel()
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	// Connections that never sent a request (clients racing http and https
	// pings) would otherwise hold Shutdown for five seconds.
	s.conns.closeNew()
	return srv.Shutdown(ctx)
}

// connTracker remembers connections that have not yet sent a request.
type connTracker struct {
	mu  sync.Mutex
	new map[net.Conn]struct{}
}

func (c *connTracker) track(conn net.Conn, st http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.new == nil {
		c.new = map[net.Conn]struct{}{}
	}
	if st == http.StateNew {
		c.new[conn] = struct{}{}
	} else {
		delete(c.new, conn)
	}
}

func (c *connTracker) closeNew() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for conn := range c.new {
		_ = conn.Close()
		delete(c.new, conn)
	}
}

// Ready reports whether the registry is listening.
func (s *Service) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return errors.New("registry not started")
	}
	return nil
}

// Operations implements emu.OperationLister.
func (s *Service) Operations() []emu.OperationInfo { return s.ops.Operations() }

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	return store.Counts(s.env.Store, map[string]string{"repositories": nsRepos, "dockerImages": nsManifests, "tags": nsTags})
}

// Reset removes every blob and upload; the store is reset by the instance.
func (s *Service) Reset(ctx context.Context) error {
	s.mu.Lock()
	s.uploads = map[string]*upload{}
	s.mu.Unlock()
	if s.blobs == nil {
		return nil
	}
	s.blobs.gc.Lock()
	defer s.blobs.gc.Unlock()
	return s.blobs.reset()
}

// EnvVars implements emu.EnvVarer (FR-CORE-004).
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	out := map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_ARTIFACTREGISTRY": "http://" + gw + "/artifactregistry/",
	}
	if reg := endpoints["ar"]; reg != "" {
		out["GCPEMU_REGISTRY"] = reg
	}
	return out
}

// RegistryAddr returns the registry listener address (host:port), for
// peers such as GKE that configure image pulls (FR-AR-005).
func (s *Service) RegistryAddr() string {
	return s.env.Endpoints.Get("ar")
}

// collect deletes blob files that are no longer referenced by any link or
// manifest.
func (s *Service) collect(digests []string) {
	if len(digests) == 0 {
		return
	}
	s.blobs.gc.Lock()
	defer s.blobs.gc.Unlock()
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, d := range digests {
			if !store.HasPrefix(tx, nsRefs, d+"|") {
				s.blobs.remove(d)
			}
		}
		return nil
	})
}
