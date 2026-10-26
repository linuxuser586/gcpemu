// Package gke emulates Google Kubernetes Engine (SRS 5.7): the
// container.googleapis.com v1 API over gRPC and REST on the gateway, backed
// by real k3s clusters running in the host's container runtime.
//
// # Control plane
//
// Clusters and node pools are created, read, listed, updated, resized,
// upgraded and deleted through container Operations (the container API's
// own Operation resource, not google.longrunning). Autopilot clusters
// (FR-GKE-012, autopilot.go) are regional, get the settings GKE sets on
// them, and report one GKE-managed node pool that the node pools API
// refuses to change; its nodes are added and removed for the cluster's
// pods (FR-GKE-015, autoscale.go). REST (gcloud, OpenTofu) is served by
// transcoding the google.api.http rules of the generated descriptors onto
// the gRPC implementation, including the legacy projects/P/zones/Z/...
// paths.
//
// # Data plane
//
// Each cluster runs one k3s server container (control plane only) and one
// privileged k3s agent container per node. Containers live on the
// cluster's VPC subnetwork (realised by compute's emu.VPC), on the
// instance's services network (to reach the emulator) and, unless the
// nodes are private, on the external network for egress. Private nodes'
// default route points at the VPC's NAT egress proxy, so they reach the
// internet only through Cloud NAT (FR-GKE-007, FR-INT-009). State lives in
// labelled volumes, so clusters survive emulator restarts with --data-dir.
// NetworkPolicy objects are enforced, by k3s's kube-router controller,
// only with Dataplane V2 or the networkPolicy setting, as on GKE
// (FR-GKE-011).
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
// # VM runtimes
//
// On a VM runtime (ADR 0005) the host cannot reach the cluster networks.
// The emulator then dials the API server and nodes through the VM relay,
// serves the API server on 127.0.0.1 and reports that as the cluster's
// endpoints, and serves LoadBalancer Services on host loopback addresses
// (lbservices.go) in place of k3s's servicelb.
package gke

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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

	mu sync.Mutex
	// bg is the current generation of background work (operations, data
	// plane, controllers). Reset cancels it, waits for it and starts the
	// next; Stop cancels it for good. The pointer is guarded by mu; a
	// generation's own fields are safe to use once read.
	bg       *background
	clusters map[string]*clusterRT // key → runtime state
	byID     map[string]string     // cluster ID → key
	busy     map[string]string     // key → running operation name
	opCancel map[string]context.CancelFunc
	started  bool
	fallback *localVPC
	// testVPC replaces compute's VPC in tests.
	testVPC emu.VPC
	// testLB replaces the load balancer in tests.
	testLB emu.LoadBalancerResources
	// hosts lists the gateway's host-routed names (real hostnames the
	// Google frontend serves, FR-INT-007); nil if the router has none.
	hosts func() []string
	// beforeNodeStart, set by tests, runs between a node container's
	// creation and its start.
	beforeNodeStart func(ctx context.Context, sn emu.SubnetNet, node, ip string)
	// beforeServerStart, set by tests, runs before a bring-up touches the
	// cluster's server container or volume.
	beforeServerStart func(ctx context.Context, key string)
	// beforeResetTeardown, set by tests, runs once Reset has started a new
	// background generation, before it tears data planes down.
	beforeResetTeardown func()
	// pubVia and pubIPs are the load balancer's public routes
	// (RoutePublic), programmed into nodes with external IPs.
	pubMu  sync.Mutex
	pubVia string
	pubIPs []string
	// loops maps cluster key/namespace/name to the loopback address a
	// LoadBalancer Service holds on a VM runtime (lbservices.go), and
	// publishedHost is the address LoadBalancer Services' Published ports
	// are served on in the emulator's container ("" is the wildcard; set
	// by tests). loopMu guards both, and is never held while calling
	// another Service.
	loopMu        sync.Mutex
	loops         map[string]string
	publishedHost string
	// hostMu guards the Services served to host clients at Loopback
	// aliases (hostsvc.go): hostSvcs by CLUSTERKEY/NAMESPACE/NAME,
	// hostNames each cluster's answered Services by namespace/name,
	// hostWarned the short names whose collision was logged, and hostFail
	// why a Service found by a host query has no alias. hostMu is taken
	// before loopMu.
	hostMu     sync.Mutex
	hostSvcs   map[string]*hostSvc
	hostNames  map[string]map[string]bool
	hostWarned map[string]bool
	hostFail   map[string]error
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{
		env:      env,
		clusters: map[string]*clusterRT{},
		byID:     map[string]string{},
		busy:     map[string]string{},
		opCancel: map[string]context.CancelFunc{},
		loops:    map[string]string{},
	}
	s.resetHost()
	s.api = &api{s: s}
	s.bg = newBackground()
	return s
}

// errHalted is the cause of a background context that Reset or Stop
// cancelled. Work stopped by it leaves the persisted status as it is.
var errHalted = errors.New("gke: background work halted by Reset or Stop")

// background is one generation of the service's background work.
type background struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
}

// bgKey keys a background context's generation (genOf).
type bgKey struct{}

func newBackground() *background {
	g := &background{}
	ctx, cancel := context.WithCancelCause(context.Background())
	g.ctx, g.cancel = context.WithValue(ctx, bgKey{}, g), cancel
	return g
}

// genOf returns the background generation ctx derives from, or nil.
func genOf(ctx context.Context) *background {
	g, _ := ctx.Value(bgKey{}).(*background)
	return g
}

// wait waits for the generation's goroutines, or for ctx to end.
func (g *background) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// track counts n goroutines into the current background generation and
// returns it, or nil if the service is stopped; each goroutine calls
// g.wg.Done when it returns.
func (s *Service) track(n int) *background {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joinLocked(s.bg, n)
}

// join counts n goroutines into generation g, unless Reset or Stop has
// halted it.
func (s *Service) join(g *background, n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joinLocked(g, n) != nil
}

// joinLocked adds n to g's goroutines unless g is halted. Reset and Stop
// cancel a generation under mu, so a halted generation gains no goroutines
// while it is waited for. Callers hold mu.
func (s *Service) joinLocked(g *background, n int) *background {
	if g.ctx.Err() != nil {
		return nil
	}
	g.wg.Add(n)
	return g
}

// halted reports whether ctx was cancelled by Reset or Stop.
func halted(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errHalted)
}

func (s *Service) Name() string { return "gke" }

// Register installs the container v1 gRPC service and its REST mapping.
func (s *Service) Register(r emu.Router) error {
	if hr, ok := r.(interface{ Hosts() []string }); ok {
		s.hosts = hr.Hosts
	}
	containerpb.RegisterClusterManagerServer(r.GRPC(), s.api)
	r.Mount("container", []string{apiHost}, s.restHandler())
	r.Mount("connectgateway", []string{connectGatewayHost}, http.HandlerFunc(s.serveConnectGateway))
	r.Handle("GET /_emu/v1/host/services", http.HandlerFunc(s.serveHostServices))
	s.registerInstanceGroups()
	return nil
}

// Start brings persisted clusters back up in the background
// (FR-CORE-031).
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	s.installHostNames()
	s.recoverClusters()
	return nil
}

// recoverClusters brings the stored clusters' data planes back in the
// background, after a restart or a State snapshot restore.
func (s *Service) recoverClusters() {
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
}

// goBackground runs f on the background context.
func (s *Service) goBackground(f func(ctx context.Context)) {
	g := s.track(1)
	if g == nil {
		return // stopped
	}
	go func() {
		defer g.wg.Done()
		f(g.ctx)
	}()
}

// Stop cancels background work. Containers are removed by the instance.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	g := s.bg
	g.cancel(errHalted)
	for _, rt := range s.clusters {
		s.stopRT(rt)
	}
	s.mu.Unlock()
	_ = g.wait(ctx)
	return nil
}

// Ready reports readiness of the API; clusters report their own status.
func (s *Service) Ready() error { return nil }

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	return store.Counts(s.env.Store, map[string]string{"clusters": nsClusters})
}

// Reset cancels the background work started before it and waits for it
// (bounded by ctx), so none of it touches a runtime object afterwards
// (#293). It then tears down every cluster's containers and volumes and
// drops the staged datastores; the store is reset by the instance. Work
// started after Reset runs on a fresh background context, unless the
// service is stopped.
func (s *Service) Reset(ctx context.Context) error {
	s.mu.Lock()
	old := s.bg
	if old.ctx.Err() == nil {
		s.bg = newBackground()
	}
	old.cancel(errHalted)
	s.mu.Unlock()
	if err := old.wait(ctx); err != nil {
		return fmt.Errorf("gke: waiting for background work: %w", err)
	}
	if s.beforeResetTeardown != nil {
		s.beforeResetTeardown()
	}
	var keys []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, r := range listClusters(tx, "") {
			keys = append(keys, r.key())
		}
		return nil
	})
	errs := []error{s.clearStaged()}
	for _, k := range keys {
		if err := s.destroyDataPlane(ctx, k); err != nil {
			errs = append(errs, err)
		}
	}
	s.mu.Lock()
	for _, rt := range s.clusters {
		s.stopRT(rt)
	}
	s.clusters = map[string]*clusterRT{}
	s.byID = map[string]string{}
	s.busy = map[string]string{}
	s.mu.Unlock()
	s.resetHost()
	return errors.Join(errs...)
}

// EnvVars implements emu.EnvVarer (FR-CORE-004): gcloud's container
// endpoint override, the kubeconfig the emulator maintains and, on a VM
// runtime, the host addresses of LoadBalancer Services.
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	out := lbEnvVars(endpoints)
	out["CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER"] = "http://" + gw + "/container/"
	out["KUBECONFIG"] = s.kubeconfigPath()
	return out
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
// endpoints under /_emu/. v1beta1 (the google-beta OpenTofu provider) has
// the same paths and a superset of the messages, so it is served as v1;
// beta-only fields are ignored.
func (s *Service) restHandler() http.Handler {
	t := newTranscoder(s.api)
	internal := s.internalHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 6 && r.URL.Path[:6] == "/_emu/" {
			internal.ServeHTTP(w, r)
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1beta1/"); ok {
			r = r.Clone(r.Context())
			r.URL.Path, r.URL.RawPath = "/v1/"+rest, ""
		}
		t.ServeHTTP(w, r)
	})
}
