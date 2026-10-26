// Package emu defines the contract between the emulator core and service
// modules (NFR-MNT-002) and the shared environment they run in.
package emu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
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

// ResetFinisher is implemented by Resetters whose background work must not
// resume before the Store is reset as well: a pass started in between would
// read the state the Reset removes (#307). The instance calls ResetDone at
// the end of every Reset, after the Store's.
type ResetFinisher interface {
	ResetDone()
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

// ResourceCounter is implemented by services that report how many
// resources they hold (admin API /_emu/v1/resources/counts, the Web
// console dashboard). Keys are user-facing resource types named like the
// API's collections ("buckets", "subscriptions"), not store namespaces.
type ResourceCounter interface {
	ResourceCounts() map[string]int
}

// OperationLister is implemented by services whose mutations run as
// Operations (admin API /_emu/v1/operations, the Web console Operations
// view). Every Service's Operations are one concept whatever their wire
// format (CONTEXT.md), so each one is summarised in the same shape.
type OperationLister interface {
	Operations() []OperationInfo
}

// OperationInfo summarises one Operation for the admin API.
type OperationInfo struct {
	// Service is the Service ID; the admin API fills it in.
	Service string `json:"service"`
	// Name is the Operation's resource name, e.g.
	// "projects/p/global/operations/operation-1".
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
	// Location is "global", a region, a zone or an API location.
	Location string `json:"location,omitempty"`
	// Type is what the Operation does, as its API names it ("insert",
	// "CREATE_CLUSTER", "change").
	Type string `json:"type,omitempty"`
	// Target is the resource the Operation acts on or produced.
	Target string `json:"target,omitempty"`
	// Status is the API's own status ("RUNNING", "pending", ...).
	Status    string          `json:"status,omitempty"`
	Done      bool            `json:"done"`
	Error     *OperationError `json:"error,omitempty"`
	StartTime time.Time       `json:"startTime,omitzero"`
	EndTime   time.Time       `json:"endTime,omitzero"`
	// Operation is the Operation as the Service's API returns it.
	Operation json.RawMessage `json:"operation,omitempty"`
}

// OperationError is why an Operation failed.
type OperationError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ParseTime parses an API timestamp (RFC 3339), returning the zero time
// for an empty or malformed one.
func ParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
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
	// Delegated returns the link the Project id is delegated to (ADR
	// 0027); nil delegates none.
	Delegated func(id string) (link string, ok bool)
	// LinkedSigningKeys returns the ID token signing keys of the Linked
	// Instances Projects are delegated to, as last heard (ADR 0027); nil
	// lists none.
	LinkedSigningKeys func() []SigningKey
	// LinkedIDToken asks the Linked Instances Projects are delegated to
	// which of them signed the ID token, for a service account of a
	// Project delegated to it (ADR 0027). It returns that account's
	// Principal, "" when none claims it, and the links it could not ask;
	// nil asks none.
	LinkedIDToken func(ctx context.Context, token string) (principal string, unreachable []string)
	// LinkedAccessToken asks the Linked Instances Projects are delegated
	// to which of them issued the access token, for a service account or
	// Federated principal of a Project delegated to it (ADR 0027). It
	// returns that Principal, "" when none claims it, and, when none
	// does, the links it could not ask, each with why, naming the link;
	// nil asks none.
	LinkedAccessToken func(ctx context.Context, token string) (principal string, unreachable map[string]error)
	// Middleware wraps HTTP handlers on per-service ports with the same
	// auth/logging the gateway applies.
	Middleware func(service string, h http.Handler) http.Handler
	// GRPCOptions returns server options (interceptors) for per-service gRPC servers.
	GRPCOptions func(service string) []grpc.ServerOption
	// Containers provides the container runtime and networks (GKE, Cloud SQL, NAT).
	Containers Containers
	// CA is the instance's local certificate authority (Section 7.4).
	CA *ca.CA
	// RequestLog records data-plane requests in the instance's request log
	// (FR-CORE-061); nil-safe for callers that check it.
	RequestLog *reqlog.Log
	// LANBindWhere locates the Instance for the --*-bind addresses
	// (LANBind); nil decides as Containerized does.
	LANBindWhere func(ctx context.Context) lanbind.Where
	// ContainerizedProbe replaces what Containerized asks, in tests; nil
	// uses runtime.InContainer and Containers.
	ContainerizedProbe *ContainerizedProbe

	// containerizedWarned holds the NoteContainerized messages logged.
	containerizedWarned sync.Map

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
// address (ListenBind) and records it in Endpoints.
func (e *Env) Listen(name string) (net.Listener, error) {
	l, err := e.ListenBind(e.Config.Port(name), true)
	if err != nil {
		return nil, err
	}
	e.Endpoints.Set(name, l.Addr().String())
	return l, nil
}

// ListenTCP listens on host:port. Port 0 picks a free port, from
// --port-range when one is set (FR-CORE-042).
func (e *Env) ListenTCP(host string, port int) (net.Listener, error) {
	lo, hi, ok, err := e.Config.ParsePortRange()
	if port != 0 || !ok || err != nil {
		return net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	// Start at a random offset so that instances sharing a range rarely
	// race for the same port.
	n := hi - lo + 1
	off := rand.IntN(n)
	for i := range n {
		p := lo + (off+i)%n
		if l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p))); err == nil {
			return l, nil
		}
	}
	return nil, fmt.Errorf("no free port on %s in range %s", host, e.Config.PortRange)
}

// Endpoints is a concurrency-safe name → address map.
type Endpoints struct {
	mu sync.RWMutex
	m  map[string]string
}

func NewEndpoints() *Endpoints { return &Endpoints{m: map[string]string{}} }

// Set records addr, a listener's address, as Dialable(addr): a Service
// endpoint is never an unspecified address (#613).
func (e *Endpoints) Set(name, addr string) {
	e.mu.Lock()
	e.m[name] = Dialable(addr)
	e.mu.Unlock()
}

// Delete removes an endpoint.
func (e *Endpoints) Delete(name string) {
	e.mu.Lock()
	delete(e.m, name)
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

// Dialable is addr, a listener's address, with an unspecified IP
// (--bind 0.0.0.0 or ::) made 127.0.0.1, which can be dialled on every OS.
// Go reports a listener on 0.0.0.0 as [::], which Windows refuses to dial.
func Dialable(addr string) string {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(h); ip != nil && ip.IsUnspecified() {
			return net.JoinHostPort("127.0.0.1", p)
		}
	}
	return addr
}

// Seeder is implemented by services that accept a section of the seed file
// (FR-CORE-011). The section is keyed by the service name. Applying a seed
// must be idempotent. baseDir resolves relative paths in the seed file.
type Seeder interface {
	ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error
}

// SeedExporter is implemented by Seeders that can write their resources
// back as their seed section (FR-CORE-013, the Web console's Seed file
// export). Applying the section to an empty Instance recreates the same
// resources. Server-assigned fields (IDs, timestamps, etags) and data that
// lives only in a data plane (Postgres rows, Kubernetes objects) are not
// exported.
type SeedExporter interface {
	// ExportSeed returns the section, which is marshalled as YAML, or nil
	// when the Service holds nothing to export.
	ExportSeed(ctx context.Context) (any, error)
}

// InvalidUniverse is the Invalid universe (see CONTEXT.md): the universe
// domain clients are given so that an API without an endpoint override is
// addressed as <api>.gcpemu.invalid, which never resolves (RFC 6761), and
// fails instead of reaching Google.
const InvalidUniverse = "gcpemu.invalid"

// EnvVarer is implemented by services that contribute client environment
// variables to `gcpemu env` (FR-CORE-004). gateway is the gateway host:port,
// endpoints the recorded listener addresses.
type EnvVarer interface {
	EnvVars(gateway string, endpoints map[string]string) map[string]string
}

// ADCKeyer is implemented by the service that issues the key file `gcpemu
// env` points GOOGLE_APPLICATION_CREDENTIALS at when gcloud's
// application-default credentials, which are Foreign credentials, would
// otherwise win over the metadata server (ADR 0010).
type ADCKeyer interface {
	// ADCKey returns a key file for the host metadata server's service
	// account. current is the previous one, returned unchanged while its
	// key is still valid.
	ADCKey(current []byte) ([]byte, error)
}

// AddressReserver is implemented by services whose stored resources record
// container addresses and ask for them again when their containers are
// recreated (a Cloud SQL instance's public and private IP). Containers
// started without a fixed address never take one of them.
type AddressReserver interface {
	ReservedAddrs() []string
}

// RelayAddresser is implemented by services that pin the address the VM
// relay holds on a container network: compute keeps the Host address of a
// VPC container network there across restarts (ADR 0014). RelayAddr
// returns "" for networks it does not pin.
type RelayAddresser interface {
	RelayAddr(network string) string
}

// LoopbackHolder is implemented by services that serve host clients on
// 127.0.0.x addresses they allocate (LB forwarding rules, Cloud SQL
// instances and GKE LoadBalancer Services on a VM runtime), so that each
// avoids the others' addresses.
type LoopbackHolder interface {
	LoopbackAddrs() []string
}

// LANFrontends is implemented by the lb Service, which serves --lb-bind
// addresses (Config.LBBind) for forwarding rules and, through it, for the
// frontends of other Services (GKE LoadBalancer Services), so that one
// claim rule covers them all: one address:port serves one frontend, the
// one with the lowest key.
type LANFrontends interface {
	// SetLANFrontends replaces the frontends owner (such as a GKE
	// cluster) registers; nil removes them.
	SetLANFrontends(owner string, fes []LANFrontend)
	// LANClaims returns the --lb-bind address:ports each of owner's
	// frontends serves, by key.
	LANClaims(owner string) map[string][]string
}

// LANFrontend is a port another Service serves on the --lb-bind
// addresses.
type LANFrontend struct {
	// Key is unique among all frontends, forwarding rules' "ip:port"
	// included; of those on one port, the lowest gets an address.
	Key string
	// Name groups a frontend's ports in endpoint "lb-lan:<Name>", and
	// What names it in messages ("LoadBalancer Service c/ns/web").
	Name, What string
	Port       int
	// Addr is the address of the frontend's own listener, if any, and
	// Wildcard is set when that listener is on the wildcard address, so
	// that it serves the --lb-bind addresses on its port itself.
	Addr     string
	Wildcard bool
	// Sig changes when Serve would serve differently.
	Sig string
	// Serve serves the connections ln accepts until ln closes.
	Serve func(ln net.Listener)
}

// ErrNoRuntime marks a permanent Containers.Runtime failure (see
// NoRuntime).
var ErrNoRuntime = errors.New("no container runtime")

// NoRuntime marks err as a permanent Containers.Runtime failure: it
// matches ErrNoRuntime as well as err, and keeps err's message.
func NoRuntime(err error) error { return noRuntime{err} }

type noRuntime struct{ error }

func (e noRuntime) Unwrap() []error { return []error{e.error, ErrNoRuntime} }

// Containers gives services lazy access to the container runtime and the
// instance's container networks (Section 3.3). It is nil-safe: services
// that need a runtime call Netplane and report not-ready on error.
type Containers interface {
	// Runtime detects the runtime on first use and reclaims orphans. An
	// error matching ErrNoRuntime is permanent: the instance has no usable
	// runtime and returns the same error on every later call. Any other
	// error is transient, and a later call tries again.
	Runtime(ctx context.Context) (*runtime.Manager, error)
	// Netplane returns the instance's services/external network manager.
	Netplane(ctx context.Context) (*netplane.Plane, error)
	// DialContext is the ContainerDialer (netplane.Plane.DialContext). It
	// dials directly until a service has used the netplane, since no
	// container can be running before then.
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	// RelayIP is the VM relay's address on the services network, at which
	// containers reach emulator endpoints (netplane.Plane.Addr), or ""
	// when no relay runs.
	RelayIP() string
}

// ConnectedRuntime is implemented by Containers that can report the
// runtime without connecting it: Connected is nil until a first Runtime
// call has connected it. Read-only views use it so that looking never
// detects the runtime or reclaims orphans.
type ConnectedRuntime interface {
	Connected() *runtime.Manager
}
