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
}

// Log is a bounded ring buffer of entries that also writes to a logger.
type Log struct {
	mu      sync.Mutex
	entries []Entry
	next    int
	full    bool
	log     *slog.Logger
}

// New returns a log keeping the last size entries.
func New(size int, log *slog.Logger) *Log {
	return &Log{entries: make([]Entry, size), log: log}
}

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
