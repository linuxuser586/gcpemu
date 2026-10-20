// Package reqlog records every API call and data-plane request as a
// structured entry (FR-CORE-061) and keeps the most recent ones in memory
// for the admin API (FR-CORE-045).
package reqlog

import (
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"
)

// Entry is one logged request.
type Entry struct {
	// ID numbers the entries of one process from 1, in the order added.
	ID       int64     `json:"id"`
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
	// NAT is the NAT egress proxy's trace of an internet egress attempt.
	NAT *NATTrace `json:"nat,omitempty"`
	// Bodies are the API call's request and response bodies. They are left
	// out of Entries and of the event stream, and read with Get.
	Bodies *Bodies `json:"-"`
}

// Bodies are the bodies of one API call.
type Bodies struct {
	Request  *Body `json:"requestBody,omitempty"`
	Response *Body `json:"responseBody,omitempty"`
}

// Detail is an entry with its bodies.
type Detail struct {
	Entry
	Bodies
}

// MaxBody is how much of a body an entry keeps.
const MaxBody = 16 << 10

// Body is the start of a request or response body.
type Body struct {
	ContentType string `json:"contentType,omitempty"`
	// Size is the body's length in bytes: what the handler read of a
	// request, or wrote of a response.
	Size int64 `json:"size"`
	// Text is the first MaxBody bytes; empty when Binary.
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Binary is set when the kept bytes are not UTF-8 text.
	Binary bool `json:"binary,omitempty"`
}

// NewBody returns the body of size bytes that starts with head, keeping
// at most MaxBody bytes of it as text.
func NewBody(contentType string, head []byte, size int64) *Body {
	b := &Body{ContentType: contentType, Size: size}
	if len(head) > MaxBody {
		head = head[:MaxBody]
	}
	b.Truncated = int64(len(head)) < size
	if b.Truncated {
		// Drop a rune the cut split.
		for i := 0; i < utf8.UTFMax-1 && len(head) > 0 && !utf8.Valid(head); i++ {
			head = head[:len(head)-1]
		}
	}
	if utf8.Valid(head) {
		b.Text = string(head)
	} else {
		b.Binary = true
	}
	return b
}

// Capture collects the first MaxBody bytes written to it and counts the rest.
type Capture struct {
	head []byte
	size int64
}

func (c *Capture) Write(p []byte) (int, error) {
	if n := MaxBody - len(c.head); n > 0 {
		c.head = append(c.head, p[:min(n, len(p))]...)
	}
	c.size += int64(len(p))
	return len(p), nil
}

// Body returns what was written as a Body, or nil when nothing was.
func (c *Capture) Body(contentType string) *Body {
	if c.size == 0 {
		return nil
	}
	return NewBody(contentType, c.head, c.size)
}

// NATTrace is one connection the NAT egress proxy saw leave a VPC
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
	lastID  int64
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

// Add records e, numbering it. A nil Log discards it.
func (l *Log) Add(e Entry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.lastID++
	e.ID = l.lastID
	l.entries[l.next] = e
	l.next = (l.next + 1) % len(l.entries)
	if l.next == 0 {
		l.full = true
	}
	l.mu.Unlock()
	if l.onAdd != nil {
		e.Bodies = nil
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
				e.Bodies = nil
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

// Get returns the retained entry numbered id, with its bodies.
func (l *Log) Get(id int64) (Detail, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.ID == id && id != 0 {
			d := Detail{Entry: e}
			if e.Bodies != nil {
				d.Bodies = *e.Bodies
			}
			d.Entry.Bodies = nil
			return d, true
		}
	}
	return Detail{}, false
}

// Clear drops all entries. IDs keep counting, so an ID never names two entries.
func (l *Log) Clear() {
	l.mu.Lock()
	clear(l.entries)
	l.next, l.full = 0, false
	l.mu.Unlock()
}
