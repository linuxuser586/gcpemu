package gke_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestImageCacheAcrossClusters is FR-GKE-006: nodes pull every image
// through the emulator's registry mirror, whose pull-through cache lives
// in the instance's data dir rather than in node volumes. An image pulled
// by one cluster therefore starts in a cluster created after the first is
// deleted, with the emulator offline and the new nodes private. The first
// pull needs the internet
// (GCPEMU_NET_TESTS=1).
func TestImageCacheAcrossClusters(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("set GCPEMU_NET_TESTS=1 to pull a public image")
	}
	t.Parallel()
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	const image = "docker.io/library/busybox:1.36.1"

	run := func(name string, private bool) {
		t.Helper()
		cl := &containerpb.Cluster{Name: name, NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}}}
		if private {
			cl.PrivateClusterConfig = &containerpb.PrivateClusterConfig{EnablePrivateNodes: true, MasterIpv4CidrBlock: "172.16.0.0/28"}
		}
		op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: cl})
		if err != nil {
			t.Fatalf("CreateCluster %s: %v", name, err)
		}
		waitOp(t, cm, zone, op.Name, 3*time.Minute)
		c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/" + name})
		if err != nil {
			t.Fatal(err)
		}
		adm, pool := adminClient(t, inst, parent+"/clusters/"+name)
		e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}
		e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "cached"},
			"spec": map[string]any{"containers": []map[string]any{{
				"name": "c", "image": image, "imagePullPolicy": "Always",
				"command": []string{"sh", "-c", "echo started; sleep 3600"},
			}}},
		}, nil)
		eventually(t, 2*time.Minute, name+": pod running "+image, func() bool {
			return strings.Contains(e.podLogs("default", "cached"), "started")
		})
		op, err = cm.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: parent + "/clusters/" + name})
		if err != nil {
			t.Fatal(err)
		}
		waitOp(t, cm, zone, op.Name, 2*time.Minute)
	}

	run("first", false)
	// No upstream registry is contacted from here on: tags are served from
	// the cache, stale if need be. The second cluster's nodes are private
	// (no route to the internet), so containerd can't fall back to pulling
	// from Docker Hub itself.
	inst.Env.Config.Offline = true
	run("second", true)
}
