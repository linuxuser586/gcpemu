package compute

import (
	"context"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// InstanceGroup is an instance group as load balancer backends see it: the
// primary internal addresses of its instances and its named ports.
type InstanceGroup struct {
	Instances  []emu.NEGEndpoint // Port is unset
	NamedPorts map[string]int
}

// InstanceGroupResolver returns the instance group at path
// ("projects/P/zones/Z/instanceGroups/N"), or ok=false when it doesn't own
// one by that name. The compute API has no instances of its own; GKE
// registers a resolver for its node pool groups.
type InstanceGroupResolver func(ctx context.Context, path string) (ig InstanceGroup, ok bool)

// AddInstanceGroupResolver registers a source of instance groups.
func (s *Service) AddInstanceGroupResolver(r InstanceGroupResolver) {
	s.chkMu.Lock()
	s.igResolvers = append(s.igResolvers, r)
	s.chkMu.Unlock()
}

// InstanceGroup returns the instance group at path or URL.
func (s *Service) InstanceGroup(ctx context.Context, group string) (InstanceGroup, error) {
	path := relPath(group)
	if !strings.Contains(path, "/instanceGroups/") {
		return InstanceGroup{}, errNotFound(path)
	}
	s.chkMu.RLock()
	rs := s.igResolvers
	s.chkMu.RUnlock()
	for _, r := range rs {
		if ig, ok := r(ctx, path); ok {
			return ig, nil
		}
	}
	return InstanceGroup{}, errNotFound(path)
}

// InstanceGroupEndpoints returns the endpoints a backend service with
// portName reaches in an instance group: every instance at the group's
// named port. A group without that named port has no endpoints, as on GCP.
func (s *Service) InstanceGroupEndpoints(ctx context.Context, group, portName string) ([]emu.NEGEndpoint, error) {
	ig, err := s.InstanceGroup(ctx, group)
	if err != nil {
		return nil, err
	}
	port, ok := ig.NamedPorts[portName]
	if !ok {
		return nil, nil
	}
	out := make([]emu.NEGEndpoint, 0, len(ig.Instances))
	for _, in := range ig.Instances {
		if in.IP == "" {
			continue
		}
		in.Port = port
		out = append(out, in)
	}
	return out, nil
}
