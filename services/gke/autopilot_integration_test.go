package gke_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// apNode is a node as the Autopilot test reads it.
type apNode struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
	} `json:"status"`
}

// apPod is a pod as the Autopilot test reads it.
type apPod struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func (e *env) apNodes() []apNode {
	var l struct{ Items []apNode }
	e.mustKube("GET", "/api/v1/nodes", nil, &l)
	return l.Items
}

func (e *env) apPods(selector string) []apPod {
	var l struct{ Items []apPod }
	e.mustKube("GET", "/api/v1/namespaces/default/pods?labelSelector="+selector, nil, &l)
	return l.Items
}

// running reports whether every pod is Running and there are want.
func running(pods []apPod, want int) bool {
	n := 0
	for _, p := range pods {
		if p.Status.Phase == "Running" {
			n++
		}
	}
	return n == want && len(pods) == want
}

func (e *env) scaleFill(replicas int) {
	e.mustKube("PATCH", "/apis/apps/v1/namespaces/default/deployments/fill", fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas), nil)
}

// TestAutopilotNodeProvisioning is FR-GKE-015 (ADR 0031): an Autopilot
// cluster starts with one node advertising the allocatable of GKE's
// ek-standard-2, whatever the host has; a pod Pending for lack of
// capacity gets a new node within 30 s (NFR-PERF-010); a compute class
// and Spot required through node affinity are labels on the node
// provisioned for them; at the host node limit pods stay Pending with
// FailedScaleUp; and emptied nodes are removed, never the last. Not
// parallel: it sets the node limit in the process environment.
func TestAutopilotNodeProvisioning(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Setenv("GCPEMU_GKE_MAX_NODES", "3")
	emutest.HoldClusterSlot(t)
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	const region = "us-central1"
	created := time.Now()
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/" + region,
		Cluster: &containerpb.Cluster{Name: "ap", Autopilot: &containerpb.Autopilot{Enabled: true}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	waitOp(t, cm, region, op.Name)
	t.Logf("Autopilot cluster RUNNING after %v (NFR-PERF-003: 60 s)", time.Since(created).Round(time.Second))
	name := "projects/" + project + "/locations/" + region + "/clusters/ap"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	// GKE's reservations on an ek-standard-2 (2 vCPU, 8 GiB): 6% of the
	// first core and 1% of the second; 25% of the first 4 GiB of memory,
	// 20% of the next 4 GiB, and the 100 MiB eviction threshold.
	nodes := e.apNodes()
	if len(nodes) != 1 {
		t.Fatalf("%d nodes at create; want 1", len(nodes))
	}
	want := map[string]string{"cpu": "1930m", "memory": "6398772Ki", "pods": "32"}
	for k, v := range want {
		if got := nodes[0].Status.Allocatable[k]; got != v {
			t.Errorf("allocatable %s = %q; want %q", k, got, v)
		}
	}
	if it := nodes[0].Metadata.Labels["node.kubernetes.io/instance-type"]; it != "ek-standard-2" {
		t.Errorf("instance type %q; want ek-standard-2", it)
	}

	// Four 600m pods need more than one node's 1930m.
	start := time.Now()
	e.mustKube("POST", "/apis/apps/v1/namespaces/default/deployments", map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "fill"},
		"spec": map[string]any{
			"replicas": 4,
			"selector": map[string]any{"matchLabels": map[string]string{"app": "fill"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app": "fill"}},
				"spec": map[string]any{"containers": []map[string]any{{
					"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent",
					"command":   []string{"sleep", "3600"},
					"resources": map[string]any{"requests": map[string]string{"cpu": "600m", "memory": "64Mi"}},
				}}},
			},
		},
	}, nil)
	eventually(t, 30*time.Second, "4 fill pods running (NFR-PERF-010)", func() bool { return running(e.apPods("app%3Dfill"), 4) })
	t.Logf("pods running on a new node after %v", time.Since(start).Round(time.Second))
	if n := len(e.apNodes()); n != 2 {
		t.Errorf("%d nodes after scale-up; want 2", n)
	}

	// A pod requiring a compute class and Spot through node affinity gets
	// a node labelled so (selectors are covered by TestNewNodeLabels).
	in := func(key, value string) map[string]any {
		return map[string]any{"key": key, "operator": "In", "values": []string{value}}
	}
	e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "scale-out", "labels": map[string]string{"app": "scale-out"}},
		"spec": map[string]any{
			"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []map[string]any{{"matchExpressions": []map[string]any{
					in("cloud.google.com/compute-class", "Scale-Out"), in("cloud.google.com/gke-spot", "true"),
				}}},
			}}},
			"containers": []map[string]any{{
				"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": []string{"sleep", "3600"},
				"resources": map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "52Mi"}},
			}},
		},
	}, nil)
	eventually(t, time.Minute, "the Scale-Out pod running", func() bool { return running(e.apPods("app%3Dscale-out"), 1) })
	var so apNode
	e.mustKube("GET", "/api/v1/nodes/"+e.apPods("app%3Dscale-out")[0].Spec.NodeName, nil, &so)
	if l := so.Metadata.Labels; l["cloud.google.com/compute-class"] != "Scale-Out" || l["cloud.google.com/gke-spot"] != "true" {
		t.Errorf("Scale-Out pod's node labels: %v", l)
	}

	// At the node limit (3), pods beyond the nodes' room stay Pending
	// with GKE's scale-up failure event.
	e.scaleFill(12)
	eventually(t, time.Minute, "a FailedScaleUp event", func() bool {
		var l struct {
			Items []struct {
				Message        string `json:"message"`
				Type           string `json:"type"`
				InvolvedObject struct {
					Name string `json:"name"`
				} `json:"involvedObject"`
			}
		}
		e.mustKube("GET", "/api/v1/namespaces/default/events?fieldSelector=reason%3DFailedScaleUp", nil, &l)
		for _, ev := range l.Items {
			if ev.Type == "Warning" && strings.HasPrefix(ev.InvolvedObject.Name, "fill-") {
				return true
			}
		}
		return false
	})
	eventually(t, time.Minute, "3 nodes at the limit", func() bool { return len(e.apNodes()) == 3 })
	time.Sleep(10 * time.Second) // a node too many would have started by now
	if n := len(e.apNodes()); n != 3 {
		t.Errorf("%d nodes at the limit; want 3", n)
	}
	pending := 0
	for _, p := range e.apPods("app%3Dfill") {
		if p.Status.Phase == "Pending" {
			pending++
		}
	}
	if pending == 0 {
		t.Error("no fill pod Pending at the node limit")
	}

	// Emptied nodes are removed, never the last.
	e.scaleFill(0)
	e.mustKube("DELETE", "/api/v1/namespaces/default/pods/scale-out?gracePeriodSeconds=0", nil, nil)
	eventually(t, 3*time.Minute, "one node left", func() bool { return len(e.apNodes()) == 1 })
	time.Sleep(45 * time.Second)
	if n := len(e.apNodes()); n != 1 {
		t.Errorf("%d nodes once empty; want 1", n)
	}
	c, err = cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if c.CurrentNodeCount != 1 {
		t.Errorf("currentNodeCount = %d; want 1", c.CurrentNodeCount)
	}
}
