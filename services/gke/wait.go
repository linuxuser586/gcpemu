package gke

import (
	"context"
	"errors"
	"fmt"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// WaitConditions implements emu.WaitConditioner (FR-CI-007).
func (s *Service) WaitConditions() map[string]emu.WaitCondition {
	return map[string]emu.WaitCondition{"gke/cluster": s.clusterWaitCondition}
}

// clusterWaitCondition is gke/cluster=PROJECT/LOCATION/NAME: met when the
// cluster is RUNNING, failed when it is in ERROR or STOPPING.
func (s *Service) clusterWaitCondition(_ context.Context, t emu.WaitTarget) (emu.WaitStatus, error) {
	if !isZone(t.Location) && !isRegion(t.Location) {
		return emu.WaitStatus{}, fmt.Errorf("location %q is not a zone or region", t.Location)
	}
	rec, err := s.loadKey(clusterKey(t.Project, t.Location, t.Name))
	if errors.Is(err, store.ErrNotFound) {
		return emu.WaitStatus{State: emu.WaitPending, Reason: "not found"}, nil
	}
	if err != nil {
		return emu.WaitStatus{State: emu.WaitPending, Reason: err.Error()}, nil
	}
	c := rec.cluster()
	reason := "status " + c.Status.String()
	switch c.Status {
	case containerpb.Cluster_RUNNING:
		return emu.WaitStatus{State: emu.WaitMet}, nil
	case containerpb.Cluster_ERROR:
		if c.StatusMessage != "" {
			reason += ": " + c.StatusMessage
		}
		return emu.WaitStatus{State: emu.WaitFailed, Reason: reason}, nil
	case containerpb.Cluster_STOPPING:
		return emu.WaitStatus{State: emu.WaitFailed, Reason: reason}, nil
	}
	return emu.WaitStatus{State: emu.WaitPending, Reason: reason}, nil
}
