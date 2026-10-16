package gke_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/emutest"
)

// FR-GKE-011: a cluster with network policy enforcement blocks pod-to-pod
// traffic a deny-all policy covers; with enforcement disabled (the nodes
// recreated) the same policy is accepted but not enforced, as on GKE's
// legacy datapath. Dataplane V2 rejects the networkPolicy setting.
func TestNetworkPolicy(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()

	_, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name:          "dpv2",
		NetworkConfig: &containerpb.NetworkConfig{DatapathProvider: containerpb.DatapathProvider_ADVANCED_DATAPATH},
		NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true, Provider: containerpb.NetworkPolicy_CALICO},
	}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("Dataplane V2 with networkPolicy: %v", err)
	}

	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name:          "netpol",
		NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true, Provider: containerpb.NetworkPolicy_CALICO},
		NodePools:     []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/netpol"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	reach := e.reachPods("a")
	eventually(t, 2*time.Minute, "client reaching the server", func() bool { return strings.Contains(reach(), "REACH-OK") })
	e.mustKube("POST", "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies", map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": "deny-all"},
		"spec":     map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Ingress"}},
	}, nil)
	eventually(t, time.Minute, "deny-all blocking the client", func() bool { return strings.Contains(reach(), "REACH-BLOCKED") })

	op, err = cm.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: name, NetworkPolicy: &containerpb.NetworkPolicy{}})
	if err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	reach = e.reachPods("b")
	eventually(t, 2*time.Minute, "client reaching the server, not enforced", func() bool { return strings.Contains(reach(), "REACH-OK") })
}

// reachPods starts a server pod and a client pod that reports each second
// whether it reaches the server, and returns the client's latest report.
func (e *env) reachPods(suffix string) func() string {
	e.t.Helper()
	web, client := "web-"+suffix, "client-"+suffix
	e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": web, "labels": map[string]string{"app": "web"}},
		"spec": map[string]any{"containers": []map[string]any{{
			"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent",
			"command": []string{"sh", "-c", "while true; do echo -e 'HTTP/1.1 200 OK\\r\\n\\r\\nok' | nc -l -p 8080; done"},
		}}},
	}, nil)
	ip := ""
	eventually(e.t, 2*time.Minute, "server pod IP", func() bool {
		var p struct {
			Status struct{ PodIP string }
		}
		e.kube("GET", "/api/v1/namespaces/default/pods/"+web, "", nil, &p)
		ip = p.Status.PodIP
		return ip != ""
	})
	e.runPod("default", client, "default", `while true; do if wget -q -T 2 -O /dev/null http://`+ip+`:8080/ 2>/dev/null; then echo REACH-OK; else echo REACH-BLOCKED; fi; sleep 1; done`)
	return func() string { return e.podLogs("default", client) }
}
