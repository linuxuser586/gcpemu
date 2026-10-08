package compute

import (
	"context"
	"net/http"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
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
// while there are any). path is the address path or URL. users computes
// them inside the same store transaction, so that concurrent changes to
// the users cannot leave a stale list behind.
func (s *Service) SetAddressUsers(ctx context.Context, path string, users func(tx store.Tx) []string) error {
	return s.env.Store.Update(func(tx store.Tx) error { return setAddressUsers(tx, relPath(path), users(tx)) })
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

// ListItem is one element of a list response with its page key (the
// resource name), for modules serving further collections (WriteList).
type ListItem struct {
	Key   string
	Value any
}

// WriteList applies filter, maxResults and pageToken exactly like this
// module's own list methods and writes a compute list response
// (kind "compute#xxxList", id the collection path).
func WriteList(w http.ResponseWriter, r *http.Request, kind, id string, items []ListItem) {
	writeList(w, r, kind, id, toListItems(items))
}

// WriteAggregated writes an aggregatedList response; scoped maps a scope
// ("global", "regions/R") to its items and field names the per-scope array.
func WriteAggregated(w http.ResponseWriter, r *http.Request, kind, id, field string, scoped map[string][]ListItem) {
	m := make(map[string][]listItem, len(scoped))
	for k, v := range scoped {
		m[k] = toListItems(v)
	}
	writeAggregated(w, r, kind, id, field, m)
}

func toListItems(in []ListItem) []listItem {
	out := make([]listItem, len(in))
	for i, it := range in {
		out[i] = listItem{key: it.Key, v: it.Value}
	}
	return out
}

// MergePatch applies a JSON merge patch to cur's JSON form and decodes the
// result into out; fields in keep retain cur's value (compute PATCH
// semantics).
func MergePatch(cur any, patch []byte, out any, keep ...string) error {
	return mergePatch(cur, patch, out, keep...)
}

// Fingerprint returns a GCP-style content fingerprint of v (ignoring its
// fingerprint fields); LabelFingerprint that of a label set.
func Fingerprint(v any) string { return fingerprint(v) }

// LabelFingerprint returns the labelFingerprint of a label set.
func LabelFingerprint(labels map[string]string) string { return labelFingerprint(labels) }

// EphemeralIP allocates an ephemeral address for a load balancer
// forwarding rule: external addresses from the global or regional pool
// (region == "" means global), internal ones from subnetwork's primary
// range, skipping reserved addresses and those for which inUse is true.
func (s *Service) EphemeralIP(ctx context.Context, project, region string, internal bool, subnetwork string, inUse func(ip string) bool) (string, error) {
	var ip string
	err := s.env.Store.Update(func(tx store.Tx) error {
		if !internal {
			pool, key := regionalPool, "extip/regional"
			if region == "" {
				pool, key = globalPool, "extip/global"
			}
			for {
				v, err := nextExternalIP(tx, key, pool)
				if err != nil {
					return err
				}
				if inUse == nil || !inUse(v) {
					ip = v
					return nil
				}
			}
		}
		if inUse == nil {
			inUse = func(string) bool { return false }
		}
		return s.allocSkipping(tx, project, region, &computev1.Address{Subnetwork: subnetwork}, inUse, &ip)
	})
	return ip, err
}

// allocSkipping scans the subnetwork range for a free internal address.
func (s *Service) allocSkipping(tx store.Tx, project, region string, a *computev1.Address, inUse func(string) bool, out *string) error {
	ref := a.Subnetwork
	if ref == "" {
		ref = "default"
	}
	sp, err := regionalRef(project, region, "subnetworks", ref)
	if err != nil {
		return errInvalidField("resource.subnetwork", a.Subnetwork, "The URL is malformed.")
	}
	sn, ok := get[computev1.Subnetwork](tx, nsSubnets, sp)
	if !ok {
		return errInvalidField("resource.subnetwork", ref, "The referenced subnetwork resource cannot be found.")
	}
	c, _ := parseCIDR(sn.IpCidrRange, true)
	used := map[uint32]bool{}
	for _, o := range list[computev1.Address](tx, nsAddresses, "projects/"+project+"/regions/"+region+"/addresses/") {
		if v, ok := parseIP(o.Address); ok {
			used[v] = true
		}
	}
	for v := c.base + 2; v < c.last(); v++ {
		if c.usable(v) && !used[v] && !inUse(ipString(v)) {
			*out = ipString(v)
			return nil
		}
	}
	return apierr.New(8, "IP space of '%s' is exhausted.", sn.SelfLink).WithLegacy("ipSpaceExhausted")
}

// AddressByIP finds the reserved address holding ip: a global one when
// region is "", else one in region. It returns the address path.
func (s *Service) AddressByIP(ctx context.Context, project, region, ip string) (string, bool) {
	ns, prefix := nsGlobalAddresses, "projects/"+project+"/global/addresses/"
	if region != "" {
		ns, prefix = nsAddresses, "projects/"+project+"/regions/"+region+"/addresses/"
	}
	var path string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, a := range list[computev1.Address](tx, ns, prefix) {
			if a.Address == ip {
				path = relPath(a.SelfLink)
				return nil
			}
		}
		return nil
	})
	return path, path != ""
}
