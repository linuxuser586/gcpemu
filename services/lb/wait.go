package lb

import (
	"context"
	"fmt"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/locations"
)

// WaitConditions implements emu.WaitConditioner (FR-CI-007).
func (s *Service) WaitConditions() map[string]emu.WaitCondition {
	return map[string]emu.WaitCondition{"lb/health": s.healthWaitCondition}
}

// healthWaitCondition is lb/health=PROJECT/LOCATION/NAME, LOCATION "global"
// or a region: met when every backend group of the backend service has a
// HEALTHY endpoint, by the health backendServices.getHealth reports. A
// service with no backends is pending.
func (s *Service) healthWaitCondition(ctx context.Context, t emu.WaitTarget) (emu.WaitStatus, error) {
	sc := scope{project: t.Project}
	if t.Location != "global" {
		if !locations.IsRegion(t.Location) {
			return emu.WaitStatus{}, fmt.Errorf("location %q is not global or a region", t.Location)
		}
		sc.region = t.Location
	}
	path := sc.coll(kindBackendService.coll) + "/" + t.Name
	obj, ok := s.load(kindBackendService, path)
	if !ok {
		return emu.WaitStatus{State: emu.WaitPending, Reason: "not found"}, nil
	}
	b := obj.(*computev1.BackendService)
	if len(b.Backends) == 0 {
		return emu.WaitStatus{State: emu.WaitPending, Reason: "no backends"}, nil
	}
	for _, be := range b.Backends {
		group := relPath(be.Group)
		if strings.HasPrefix(group, "zones/") || strings.HasPrefix(group, "regions/") {
			group = "projects/" + t.Project + "/" + group
		}
		if !anyHealthy(s.dp.groupHealth(ctx, path, group)) {
			short := strings.TrimPrefix(group, "projects/"+t.Project+"/")
			return emu.WaitStatus{State: emu.WaitPending, Reason: "group " + short + " has no HEALTHY endpoint"}, nil
		}
	}
	return emu.WaitStatus{State: emu.WaitMet}, nil
}

func anyHealthy(hs []*computev1.HealthStatus) bool {
	for _, h := range hs {
		if h.HealthState == "HEALTHY" {
			return true
		}
	}
	return false
}
