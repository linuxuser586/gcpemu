// Package reqlog records every API call and data-plane request as a
// structured entry (FR-CORE-061) and keeps the most recent ones in memory
// for the admin API (FR-CORE-045).
package reqlog

import (
	"log/slog"
	"sync"
	"time"
)

// Entry is one logged request.
type Entry struct {
	Time     time.Time `json:"time"`
	Service  string    `json:"service"`
	Protocol string    `json:"protocol"` // http, grpc, dns, postgres, tcp, udp
	Method   string    `json:"method"`
	// Resource is the resource acted on: the one whose permission an API
	// call checked first, the answering DNS zone, the Cloud SQL instance or
	// the Cloud NAT gateway.
	Resource  string `json:"resource,omitempty"`
	Principal string `json:"principal,omitempty"`
	// Status is protocol-specific: the HTTP status, gRPC code or DNS
	// rcode; for connections (postgres, NAT) 0 when admitted and 1 when
	// refused, with the reason in Code.
	Status    int     `json:"status"`
	Code      string  `json:"code,omitempty"`
	LatencyMS float64 `json:"latencyMs"`
	// LB is the load balancer's trace of a data-plane request.
	LB *LBTrace `json:"lb,omitempty"`
	// NAT is the egress gateway's trace of an internet egress attempt.
	NAT *NATTrace `json:"nat,omitempty"`
}

// NATTrace is one connection the NAT egress gateway saw leave a VPC
// network: translated, dropped or answered by the offline sink (the
// Entry's Code). The Cloud NAT gateway, or the network when no NAT covers
// the source, is the Entry's Resource. Fields follow Cloud NAT's log
// (FR-NAT-004).
type NATTrace struct {
	Network    string `json:"network"`
	Subnetwork string `json:"subnetwork,omitempty"`
	// VM is the workload holding the source address, e.g. a GKE node.
	VM       string `json:"vm,omitempty"`
	SrcIP    string `json:"srcIp"`
	SrcPort  int    `json:"srcPort"`
	DestIP   string `json:"destIp"`
	DestPort int    `json:"destPort"`
	// NatIP and NatPort are the translated source, when one was allocated.
	NatIP   string `json:"natIp,omitempty"`
	NatPort int    `json:"natPort,omitempty"`
	// Request is the first line the offline sink received.
	Request string `json:"request,omitempty"`
}

// LBTrace is how a load balancer served one request: the forwarding rule
// is the Entry's Resource.
type LBTrace struct {
	Host string `json:"host"`
	// Path is the path and query as the client sent them.
	Path        string `json:"path"`
	TargetProxy string `json:"targetProxy,omitempty"`
	URLMap      string `json:"urlMap,omitempty"`
	// Match is the URL map rule that matched: "routeRule", "pathRule",
	// "pathMatcherDefault" or "urlMapDefault"; empty when the URL map is
	// missing or invalid.
	Match       string `json:"match,omitempty"`
	PathMatcher string `json:"pathMatcher,omitempty"`
	// Priority is the matched route rule's; Paths the matched path rule's.
	Priority *int64   `json:"priority,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	// Redirect is the Location of a redirect the load balancer answered.
	Redirect string `json:"redirect,omitempty"`
	// Backend is the backend service or bucket path, Endpoint the
	// endpoint (ip:port, or the bucket) that answered.
	Backend  string `json:"backend,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// CacheStatus is Cloud CDN's {cdn_cache_status}: hit, miss,
	// revalidated, uncacheable, or disabled when the backend has no CDN.
	CacheStatus      string  `json:"cacheStatus,omitempty"`
	StatusDetails    string  `json:"statusDetails,omitempty"`
	BackendLatencyMS float64 `json:"backendLatencyMs,omitempty"`
}

// Log is a bounded ring buffer of entries that also writes to a logger.
type Log struct {
	mu      sync.Mutex
	entries []Entry
	next    int
	full    bool
	log     *slog.Logger
	onAdd   func(Entry)
}

// New returns a log keeping the last size entries.
func New(size int, log *slog.Logger) *Log {
	return &Log{entries: make([]Entry, size), log: log}
}

// OnAdd sets a function called with every entry added. Set it before the
// Log is used.
func (l *Log) OnAdd(fn func(Entry)) { l.onAdd = fn }

// Add records e. A nil Log discards it.
func (l *Log) Add(e Entry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.entries[l.next] = e
	l.next = (l.next + 1) % len(l.entries)
	if l.next == 0 {
		l.full = true
	}
	l.mu.Unlock()
	if l.onAdd != nil {
		l.onAdd(e)
	}
	if l.log != nil {
		l.log.Info("request",
			"service", e.Service, "protocol", e.Protocol, "method", e.Method,
			"resource", e.Resource, "principal", e.Principal,
			"status", e.Status, "code", e.Code, "latencyMs", e.LatencyMS)
	}
}

// Entries returns the retained entries, oldest first, optionally filtered by service.
func (l *Log) Entries(service string) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	appendRange := func(es []Entry) {
		for _, e := range es {
			if service == "" || e.Service == service {
				out = append(out, e)
			}
		}
	}
	if l.full {
		appendRange(l.entries[l.next:])
	}
	appendRange(l.entries[:l.next])
	return out
}

// Clear drops all entries.
func (l *Log) Clear() {
	l.mu.Lock()
	clear(l.entries)
	l.next, l.full = 0, false
	l.mu.Unlock()
}
