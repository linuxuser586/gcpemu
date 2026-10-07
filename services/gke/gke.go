// Package gke emulates Google Kubernetes Engine (SRS 5.7): the
// container.googleapis.com v1 API over gRPC and REST on the gateway, backed
// by real k3s clusters running in the host's container runtime.
//
// # Control plane
//
// Clusters (Standard mode) and node pools are created, read, listed,
// updated, resized, upgraded and deleted through container Operations
// (the container API's own Operation resource, not google.longrunning).
// Autopilot clusters are rejected as UNIMPLEMENTED (FR-GKE-012). REST
// (gcloud, OpenTofu) is served by transcoding the google.api.http rules of
// the generated descriptors onto the gRPC implementation, including the
// legacy projects/P/zones/Z/... paths.
//
// # Data plane
//
// Each cluster runs one k3s server container (control plane only) and one
// privileged k3s agent container per node. Containers live on the
// cluster's VPC subnetwork (realised by compute's emu.VPC), on the
// instance's services network (to reach the emulator) and, unless the
// nodes are private, on the external network for egress. Private nodes'
// default route points at the VPC egress gateway, so they reach the
// internet only through Cloud NAT (FR-GKE-007, FR-INT-009). State lives in
// labelled volumes, so clusters survive emulator restarts with --data-dir.
//
// Every node runs the "gke-node" agent (the gcpemu binary) as its
// entrypoint. It serves the GKE metadata server on 169.254.169.254
// (Workload Identity, FR-GKE-005), relays DNS to the emulated Cloud DNS
// (FR-INT-005), proxies Artifact Registry pulls with the node service
// account's token (FR-INT-006) and then runs the k3s agent.
//
// # Real hostnames in pods (FR-INT-007)
//
// Unmodified client libraries in pods reach emulated services by their
// real hostnames with no endpoint configuration:
//
//   - DNS: the node relay (CoreDNS's upstream) answers every hostname the
//     gateway mounts (storage.googleapis.com, pubsub.googleapis.com,
//     sqladmin.googleapis.com, ...) and LOCATION-docker.pkg.dev with
//     169.254.169.254; other names resolve through the emulated Cloud DNS.
//   - TLS: the node relays 169.254.169.254:443 to the emulator's Google
//     frontend (internal/frontend), which terminates TLS with a
//     certificate from the instance CA and passes HTTP/1.1, HTTP/2 and
//     gRPC through to the gateway with Host / :authority intact.
//   - Trust: nodes add the CA to their system bundle, and a mutating
//     admission webhook served by the emulator (cainject.go) mounts a
//     bundle of the host's system roots plus the emulator CA into every
//     pod at /etc/ssl/certs/ca-certificates.crt and sets SSL_CERT_FILE,
//     except in kube-system and in namespaces or pods labelled
//     gcpemu.dev/inject=disabled.
//
// The API server authenticates bearer tokens through a TokenReview webhook
// served by the emulator (emulator IAM tokens, FR-GKE-004) and authorizes
// with Node,RBAC,Webhook where the webhook grants requests the caller's
// IAM roles (container.admin/developer/viewer) allow.
//
// GKE requires Linux hosts (NFR-PORT-003).
package gke

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	goruntime "runtime"
	"strconv"
	"sync"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// apiHost is the real API host (host mode).
const apiHost = "container.googleapis.com"

// maxNodesEnv overrides the per-instance node limit (FR-GKE-003).
const maxNodesEnv = "GCPEMU_GKE_MAX_NODES"

// defaultMaxNodes is the default per-instance node limit.
const defaultMaxNodes = 5

// Service is the GKE service module.
type Service struct {
	env *emu.Env
	api *api

	// ctx lives from Start to Stop; background work (operations, data
	// plane, NEG sync) derives from it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	clusters map[string]*clusterRT // key → runtime state
	byID     map[string]string     // cluster ID → key
	busy     map[string]string     // key → running operation name
	opCancel map[string]context.CancelFunc
	started  bool
	fallback *localVPC
	// testVPC replaces compute's VPC in tests.
	testVPC emu.VPC
	// hosts lists the gateway's host-routed names (real hostnames the
	// Google frontend serves, FR-INT-007); nil if the router has none.
	hosts func() []string
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{
		env:      env,
		clusters: map[string]*clusterRT{},
		byID:     map[string]string{},
		busy:     map[string]string{},
		opCancel: map[string]context.CancelFunc{},
	}
	s.api = &api{s: s}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

func (s *Service) Name() string { return "gke" }

// Register installs the container v1 gRPC service and its REST mapping.
func (s *Service) Register(r emu.Router) error {
	if hr, ok := r.(interface{ Hosts() []string }); ok {
		s.hosts = hr.Hosts
	}
	containerpb.RegisterClusterManagerServer(r.GRPC(), s.api)
	r.Mount("container", []string{apiHost}, s.restHandler())
	r.Mount("serviceusage", []string{"serviceusage.googleapis.com"}, serviceUsageHandler())
	return nil
}

// Start rejects non-Linux hosts and brings persisted clusters back up in
// the background (FR-CORE-031).
func (s *Service) Start(ctx context.Context) error {
	if goruntime.GOOS != "linux" {
		return errors.New("GKE is only supported on Linux hosts in this release (NFR-PORT-003); run gcpemu without the gke service on " + goruntime.GOOS)
	}
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	var recs []*clusterRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs = listClusters(tx, "")
		return nil
	})
	s.abortStaleOps()
	for _, r := range recs {
		c := r.cluster()
		s.index(r.key(), c.Id)
		switch c.Status {
		case containerpb.Cluster_PROVISIONING, containerpb.Cluster_RUNNING, containerpb.Cluster_RECONCILING, containerpb.Cluster_ERROR:
			key := r.key()
			s.markRestoring(key)
			s.goBackground(func(ctx context.Context) { s.restore(ctx, key) })
		case containerpb.Cluster_STOPPING:
			key := r.key()
			s.goBackground(func(ctx context.Context) { _ = s.teardown(ctx, key) })
		}
	}
	return nil
}

// goBackground runs f on the service context.
func (s *Service) goBackground(f func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f(s.ctx)
	}()
}

// Stop cancels background work. Containers are removed by the instance.
func (s *Service) Stop(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// Ready reports readiness of the API; clusters report their own status.
func (s *Service) Ready() error {
	if goruntime.GOOS != "linux" {
		return fmt.Errorf("GKE requires Linux (NFR-PORT-003)")
	}
	return nil
}

// Reset tears down every cluster's containers and volumes; the store is
// reset by the instance.
func (s *Service) Reset(ctx context.Context) error {
	var keys []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, r := range listClusters(tx, "") {
			keys = append(keys, r.key())
		}
		return nil
	})
	var errs []error
	for _, k := range keys {
		if err := s.destroyDataPlane(ctx, k); err != nil {
			errs = append(errs, err)
		}
	}
	s.mu.Lock()
	for _, rt := range s.clusters {
		rt.stop()
	}
	s.clusters = map[string]*clusterRT{}
	s.byID = map[string]string{}
	s.busy = map[string]string{}
	s.mu.Unlock()
	return errors.Join(errs...)
}

// EnvVars implements emu.EnvVarer (FR-CORE-004): gcloud's container
// endpoint override and the kubeconfig the emulator maintains.
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	return map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER":    "http://" + gw + "/container/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_SERVICEUSAGE": "http://" + gw + "/serviceusage/",
		"KUBECONFIG": s.kubeconfigPath(),
	}
}

// maxNodes is the per-instance node limit.
func maxNodes() int {
	if v, err := strconv.Atoi(os.Getenv(maxNodesEnv)); err == nil && v > 0 {
		return v
	}
	return defaultMaxNodes
}

// index records a cluster ID → key mapping (webhook and agent URLs carry
// the ID).
func (s *Service) index(key, id string) {
	s.mu.Lock()
	s.byID[id] = key
	s.mu.Unlock()
}

func (s *Service) keyOf(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	return k, ok
}

// restHandler serves container v1 REST plus the emulator-internal
// endpoints under /_emu/.
func (s *Service) restHandler() http.Handler {
	t := newTranscoder(s.api)
	internal := s.internalHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 6 && r.URL.Path[:6] == "/_emu/" {
			internal.ServeHTTP(w, r)
			return
		}
		t.ServeHTTP(w, r)
	})
}
