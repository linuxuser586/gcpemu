// Package emu defines the contract between the emulator core and service
// modules (NFR-MNT-002) and the shared environment they run in.
package emu

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Service is one emulated GCP service.
type Service interface {
	// Name is the short service name used by --services (e.g. "gcs").
	Name() string
	// Register mounts REST handlers and gRPC services on the gateway.
	Register(r Router) error
	// Start starts the service's own listeners and background work.
	Start(ctx context.Context) error
	// Stop releases everything Start acquired.
	Stop(ctx context.Context) error
	// Ready returns nil when the service can take traffic, or the reason it can't.
	Ready() error
}

// Resetter is implemented by services that hold state outside the Store
// (blob files, in-memory queues) and must clear it on `gcpemu reset`.
type Resetter interface {
	Reset(ctx context.Context) error
}

// Router is the gateway surface a service registers on (FR-CORE-040).
type Router interface {
	// Mount serves h for the API: under the path prefix "/<api>/" (prefix
	// stripped), and at the root for requests whose Host matches one of hosts.
	Mount(api string, hosts []string, h http.Handler)
	// Handle registers an unambiguous root path pattern (net/http.ServeMux syntax).
	Handle(pattern string, h http.Handler)
	// GRPC returns the gateway's gRPC server.
	GRPC() *grpc.Server
	// Fallback serves h for gateway paths no other route matches (used by
	// GCS for XML-style /BUCKET/OBJECT reads). Only one service may set it.
	Fallback(h http.Handler)
}

// ClockObserver is implemented by services with time-driven behaviour
// (lifecycle rules, TTLs) that must re-evaluate after `gcpemu time advance`.
type ClockObserver interface {
	ClockAdvanced(ctx context.Context) error
}

// Env is the shared environment passed to every service.
type Env struct {
	Config *config.Config
	Store  store.Store
	Clock  clock.Clock
	IDs    *IDs
	Log    *slog.Logger
	// DataDir holds per-service file data (objects, blobs, caches).
	DataDir string
	// Auth authorizes callers (FR-IAM-004); never nil.
	Auth Authorizer
	// Endpoints records listener addresses for endpoints.json.
	Endpoints *Endpoints
	// Middleware wraps HTTP handlers on per-service ports with the same
	// auth/logging the gateway applies.
	Middleware func(service string, h http.Handler) http.Handler
	// GRPCOptions returns server options (interceptors) for per-service gRPC servers.
	GRPCOptions func(service string) []grpc.ServerOption
	// Containers provides the container runtime and networks (GKE, Cloud SQL, NAT).
	Containers Containers

	mu       sync.RWMutex
	services map[string]Service
}

// SetServices installs the running service set for Lookup.
func (e *Env) SetServices(s map[string]Service) {
	e.mu.Lock()
	e.services = s
	e.mu.Unlock()
}

// Lookup returns a running service by name.
func (e *Env) Lookup(name string) (Service, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.services[name]
	return s, ok
}

// ServiceDir returns (creating) the data directory for a service.
func (e *Env) ServiceDir(name string) (string, error) {
	d := filepath.Join(e.DataDir, name)
	return d, os.MkdirAll(d, 0o700)
}

// Listen opens a TCP listener for a named port on the configured bind
// address and records it in Endpoints.
func (e *Env) Listen(name string) (net.Listener, error) {
	addr := net.JoinHostPort(e.Config.Bind, strconv.Itoa(e.Config.Port(name)))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	e.Endpoints.Set(name, l.Addr().String())
	return l, nil
}

// Endpoints is a concurrency-safe name → address map.
type Endpoints struct {
	mu sync.RWMutex
	m  map[string]string
}

func NewEndpoints() *Endpoints { return &Endpoints{m: map[string]string{}} }

func (e *Endpoints) Set(name, addr string) {
	e.mu.Lock()
	e.m[name] = addr
	e.mu.Unlock()
}

func (e *Endpoints) Get(name string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.m[name]
}

// All returns a copy of the map.
func (e *Endpoints) All() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]string, len(e.m))
	for k, v := range e.m {
		out[k] = v
	}
	return out
}

// Names returns the endpoint names in sorted order.
func (e *Endpoints) Names() []string {
	all := e.All()
	out := make([]string, 0, len(all))
	for k := range all {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Seeder is implemented by services that accept a section of the seed file
// (FR-CORE-011). The section is keyed by the service name. Applying a seed
// must be idempotent. baseDir resolves relative paths in the seed file.
type Seeder interface {
	ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error
}

// EnvVarer is implemented by services that contribute client environment
// variables to `gcpemu env` (FR-CORE-004). gateway is the gateway host:port,
// endpoints the recorded listener addresses.
type EnvVarer interface {
	EnvVars(gateway string, endpoints map[string]string) map[string]string
}

// Containers gives services lazy access to the container runtime and the
// instance's container networks (Section 3.3). It is nil-safe: services
// that need a runtime call Netplane and report not-ready on error.
type Containers interface {
	// Runtime detects the runtime on first use and reclaims orphans.
	Runtime(ctx context.Context) (*runtime.Manager, error)
	// Netplane returns the instance's services/external network manager.
	Netplane(ctx context.Context) (*netplane.Plane, error)
}
