package gke

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// infoOnly is a container runtime that reports its info and offers no
// netplane, so bring-ups fail.
type infoOnly struct {
	emu.Containers
	info runtime.Info
}

func (c *infoOnly) Runtime(context.Context) (*runtime.Manager, error) {
	return &runtime.Manager{Info: c.info}, nil
}

func (c *infoOnly) Netplane(context.Context) (*netplane.Plane, error) {
	return nil, errors.New("no netplane")
}

// On a runtime whose containers lack the cpuset cgroup controller, cluster
// and node pool create fail fast with FAILED_PRECONDITION and store
// nothing, since k3s nodes would exit at once (#632); on one that has it
// they are accepted.
func TestCreateWithoutCPUSet(t *testing.T) {
	a := newAPI(t)
	rt := &infoOnly{info: runtime.Info{Name: "podman", Version: "6.1.4", NoCPUSet: true}}
	a.s.env.Containers = rt
	ctx := context.Background()
	const parent = "projects/cpuset-test/locations/us-central1-a"
	_, err := a.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1", InitialNodeCount: 1}})
	if err == nil {
		t.Fatal("create cluster: accepted")
	}
	wantCode(t, "create cluster", err, codes.FailedPrecondition)
	if msg := apierr.From(err).Message; !strings.Contains(msg, "cpuset") || !strings.Contains(msg, "GKE nodes (k3s)") {
		t.Errorf("create cluster message %q", msg)
	}
	_, err = a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/c1"})
	wantCode(t, "get refused cluster", err, codes.NotFound)

	rt.info.NoCPUSet = false
	createRunning(t, a, parent, &containerpb.Cluster{Name: "c1", InitialNodeCount: 1})

	rt.info.NoCPUSet = true
	_, err = a.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: parent + "/clusters/nope", NodePool: &containerpb.NodePool{Name: "extra", InitialNodeCount: 1}})
	wantCode(t, "create node pool in a missing cluster", err, codes.NotFound)
	_, err = a.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: parent + "/clusters/c1", NodePool: &containerpb.NodePool{Name: "extra", InitialNodeCount: 1}})
	wantCode(t, "create node pool", err, codes.FailedPrecondition)
	_, err = a.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: parent + "/clusters/c1/nodePools/extra"})
	wantCode(t, "get refused pool", err, codes.NotFound)
}
