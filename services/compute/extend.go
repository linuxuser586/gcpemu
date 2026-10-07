package compute

import (
	"context"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Extension points for modules that serve further compute collections on
// the same API (the M3 load balancer: forwarding rules, proxies, URL maps,
// backend services, health checks, SSL certificates). They obtain the
// *Service with env.Lookup("compute") and register routes with HandleFunc,
// resource-in-use checks with AddUsageChecker, and use the helpers below so
// their operations, selfLinks and timestamps match this module's.

// StartOperation records a compute Operation (global, "regions/R" or
// "zones/Z" scope) whose work is fn, honouring the configured LRO latency
// (FR-CORE-023). Validation should happen before; errors from fn are
// reported in the operation.
func (s *Service) StartOperation(ctx context.Context, project, scope, opType, target string, targetID uint64, fn func(ctx context.Context) error) (*computev1.Operation, error) {
	return s.startOp(ctx, opSpec{project: project, scope: scope, opType: opType, target: relPath(target), targetID: targetID}, fn)
}

// SetAddressUsers records which resources use an address (status IN_USE
// while users is non-empty). path is the address path or URL.
func (s *Service) SetAddressUsers(ctx context.Context, path string, users []string) error {
	return s.env.Store.Update(func(tx store.Tx) error { return setAddressUsers(tx, relPath(path), users) })
}

// Address returns a regional or global address by path or URL.
func (s *Service) Address(ctx context.Context, path string) (*computev1.Address, error) {
	p := relPath(path)
	ns := nsAddresses
	if _, loc, _ := pathParts(p); loc == "global" {
		ns = nsGlobalAddresses
	}
	var a *computev1.Address
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { a, ok = get[computev1.Address](tx, ns, p); return nil })
	if !ok {
		return nil, errNotFound(p)
	}
	return a, nil
}

// SelfLink returns the compute selfLink of a relative resource path.
func SelfLink(path string) string { return link(relPath(path)) }

// Timestamp renders the current emulator time as a compute creationTimestamp.
func (s *Service) Timestamp() string { return stamp(s.env.Clock.Now()) }
