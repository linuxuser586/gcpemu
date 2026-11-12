package lb

import (
	"context"
	"sort"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// NEGEndpointHealth implements emu.NEGHealth for GKE's NEG readiness gate
// (ADR 0071): the endpoint ip:port's state in each backend service with a
// health check that has the zonal NEG as a backend, by relative name, as
// getHealth reports it. Every backend service counts, whether or not a
// forwarding rule routes to it.
func (s *Service) NEGEndpointHealth(ctx context.Context, project, zone, neg, ip string, port int) []emu.NEGBackendHealth {
	group := "projects/" + project + "/zones/" + zone + "/networkEndpointGroups/" + neg
	cfg := s.dp.cfg.Load()
	paths := make([]string, 0, len(cfg.svcs))
	for p := range cfg.svcs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []emu.NEGBackendHealth
	for _, p := range paths {
		b := cfg.svcs[p]
		if b.hcfg.Load() == nil || !usesGroup(p, b, group) {
			continue
		}
		h := emu.NEGBackendHealth{BackendService: p}
		for _, hs := range s.dp.groupHealth(ctx, p, group) {
			if hs.IpAddress == ip && hs.Port == int64(port) {
				h.State = hs.HealthState
			}
		}
		out = append(out, h)
	}
	return out
}

// usesGroup reports whether the backend service at path has group
// ("projects/P/zones/Z/networkEndpointGroups/N") as a backend.
func usesGroup(path string, b *backendSvc, group string) bool {
	c := b.conf()
	if c == nil {
		return false
	}
	for _, be := range c.Backends {
		g := relPath(be.Group)
		if strings.HasPrefix(g, "zones/") || strings.HasPrefix(g, "regions/") {
			g = "projects/" + scopeOfPath(path).project + "/" + g
		}
		if g == group {
			return true
		}
	}
	return false
}
