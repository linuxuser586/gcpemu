//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

// TestAutopilot is the SRS 11.4 Autopilot acceptance (v1.0). For each
// supported minor (FR-GKE-002), at its newest version on the Rapid
// channel:
//
//  1. tofu apply creates an Autopilot cluster from enable_autopilot = true
//     with allow_net_admin (autopilot/), and a plan right after shows no
//     changes;
//  2. Kubernetes conformance passes (Sonobuoy quick mode);
//  3. tofu destroy leaves no containers.
//
// The cluster at the default version's minor also runs the workload
// checks between 1 and 2:
//
//   - a Deployment scales past one node (FR-GKE-015);
//   - the Workload restrictions refuse a privileged pod (FR-GKE-016);
//   - Istio's sidecar injection works with allowNetAdmin: the injected pod
//     runs, and its proxy, which Istio is configured to leave without
//     requests, gets Autopilot's defaults. The defaults come from the
//     resources webhook running again after the injector added the proxy
//     (reinvocationPolicy IfNeeded), since it runs first (#665).
//
// It needs internet access (GCPEMU_NET_TESTS=1) for Sonobuoy, the
// Kubernetes conformance images, Helm and Istio.
func TestAutopilot(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("GCPEMU_NET_TESTS != 1: Sonobuoy, the Kubernetes conformance images and Istio come from the internet")
	}
	tofuBin, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("OpenTofu (tofu) is not on PATH")
	}
	inst := emutest.Start(t, []string{"compute", "gke"})
	gw := inst.Endpoint("gateway")

	var sc struct {
		DefaultClusterVersion string   `json:"defaultClusterVersion"`
		ValidMasterVersions   []string `json:"validMasterVersions"`
		Channels              []struct {
			Channel       string   `json:"channel"`
			ValidVersions []string `json:"validVersions"`
		} `json:"channels"`
	}
	if !getJSON(gw, "/container/v1/projects/autopilot/locations/us-central1/serverConfig", &sc) {
		t.Fatal("getServerConfig failed")
	}
	// The supported minors are the server's (FR-GKE-002); the newest
	// version of each, as valid versions are newest first, must be on the
	// Rapid channel the module creates clusters on.
	var newest, rapid []string
	for _, v := range sc.ValidMasterVersions {
		if !slices.ContainsFunc(newest, func(n string) bool { return minorOf(n) == minorOf(v) }) {
			newest = append(newest, v)
		}
	}
	for _, ch := range sc.Channels {
		if ch.Channel == "RAPID" {
			rapid = ch.ValidVersions
		}
	}
	if len(newest) != 3 {
		t.Fatalf("supported versions %v: want three minors", sc.ValidMasterVersions)
	}
	for _, v := range newest {
		if !slices.Contains(rapid, v) {
			t.Fatalf("%s is not on the Rapid channel (%v)", v, rapid)
		}
	}
	for _, v := range newest {
		t.Run(minorOf(v), func(t *testing.T) {
			autopilotCluster(t, inst, tofuBin, v, minorOf(v) == minorOf(sc.DefaultClusterVersion))
		})
	}
}

// minorOf returns the minor of a GKE version ("1.36.5-gke.100" → "1.36").
func minorOf(v string) string {
	p := strings.SplitN(v, ".", 3)
	return p[0] + "." + p[1]
}

// autopilotCluster runs steps 1–3 for one version, and the workload
// checks if workloads.
func autopilotCluster(t *testing.T, inst *emutest.Instance, tofuBin, version string, workloads bool) {
	tf := newTofu(t, tofuBin, inst.Endpoint("gateway"), "autopilot")
	tf.run(10*time.Minute, "init", "-no-color")
	destroyed := false
	t.Cleanup(func() {
		if !destroyed {
			_, _ = tf.try(15*time.Minute, "destroy", "-auto-approve")
		}
	})
	tf.apply(map[string]string{"kubernetes_version": version})
	tf.planClean()
	out := tf.outputs()
	if out["master_version"] != version {
		t.Errorf("master_version = %q, want %q", out["master_version"], version)
	}
	k := newKube(t, inst.Endpoint("gateway"), out["cluster"])

	if workloads {
		t.Run("scale_past_one_node", func(t *testing.T) { scalePastOneNode(t, k.on(t)) })
		t.Run("privileged_pod_rejected", func(t *testing.T) { privilegedRejected(t, k.on(t)) })
		t.Run("istio_sidecar_injection", func(t *testing.T) { istioInjection(t, k.on(t)) })
	}
	t.Run("kubernetes_conformance", func(t *testing.T) { kubernetesConformance(t, k.on(t), strings.SplitN(version, "-", 2)[0]) })

	tf.run(15*time.Minute, "destroy", "-auto-approve")
	destroyed = true
	var left []string
	hosttest.Eventually(t, time.Minute, "no leftover containers", func() bool {
		cts, err := inst.Internal().Containers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		left = leftoverContainers(cts)
		return len(left) == 0
	}, func() string { return strings.Join(left, "\n") })
}

// on returns k for use in test t.
func (k *kube) on(t *testing.T) *kube {
	c := *k
	c.t = t
	return &c
}

// pauseImage is preloaded on every node (systemImages in services/gke).
const pauseImage = "rancher/mirrored-pause:3.10.2"

// apPod is a pod as the Autopilot test reads it.
type apPod struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string        `json:"nodeName"`
		InitContainers []apContainer `json:"initContainers"`
		Containers     []apContainer `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type apContainer struct {
	Name      string `json:"name"`
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
}

func (k *kube) pods(ns, selector string) []apPod {
	var l struct{ Items []apPod }
	k.get("/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape(selector), &l)
	return l.Items
}

// deployment is a Deployment of replicas pods labelled app=name running
// containers.
func deployment(ns, name string, replicas int, containers ...map[string]any) map[string]any {
	return obj("Deployment", ns, name, nil, map[string]any{"spec": map[string]any{
		"replicas": replicas,
		"selector": map[string]any{"matchLabels": map[string]string{"app": name}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]string{"app": name}},
			"spec":     map[string]any{"containers": containers},
		},
	}})
}

// scalePastOneNode runs four pods with Autopilot's default requests
// (0.5 vCPU each), more than the first node's 1930m allocatable holds,
// and expects them spread over new nodes.
func scalePastOneNode(t *testing.T, k *kube) {
	k.apply(deployment("default", "fill", 4, map[string]any{"name": "c", "image": pauseImage}))
	var nodes []string
	hosttest.Eventually(t, 3*time.Minute, "4 fill pods running on more than one node", func() bool {
		nodes = nil
		running := 0
		for _, p := range k.pods("default", "app=fill") {
			if p.Status.Phase == "Running" {
				running++
				if !slices.Contains(nodes, p.Spec.NodeName) {
					nodes = append(nodes, p.Spec.NodeName)
				}
			}
		}
		return running == 4 && len(nodes) > 1
	}, func() string { return "running on " + strings.Join(nodes, ", ") })
	t.Logf("fill pods on nodes %v", nodes)
	k.delete("Deployment", "default", "fill")
}

// privilegedRejected expects the Workload restrictions to refuse a
// privileged pod, with GKE's message.
func privilegedRejected(t *testing.T, k *kube) {
	b, _ := json.Marshal(obj("Pod", "default", "priv", nil, map[string]any{"spec": map[string]any{"containers": []map[string]any{{
		"name": "c", "image": pauseImage, "securityContext": map[string]any{"privileged": true}}}}}))
	code, resp := k.do(http.MethodPost, "/api/v1/namespaces/default/pods", "application/json", b, nil)
	if code != http.StatusForbidden || !strings.Contains(string(resp), "GKE Warden rejected the request") ||
		!strings.Contains(string(resp), "container c is privileged; not allowed in Autopilot") {
		t.Fatalf("privileged pod: %d %s; want 403 from the Workload restrictions", code, resp)
	}
}

// istiodValues leaves the injected proxy without requests, so that
// Autopilot sets its defaults.
const istiodValues = `global:
  proxy:
    resources: null
`

// istioInjection installs Istio, runs a pod in a namespace with sidecar
// injection, and checks the injected proxy's requests.
func istioInjection(t *testing.T, k *kube) {
	installIstioCharts(t, k,
		[]string{"istio-base", "base"},
		[]string{"istiod", "istio-control/istio-discovery", "-f", valuesFile(t, istiodValues)})
	k.apply(map[string]any{"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "mesh", "labels": map[string]string{"istio-injection": "enabled"}}})
	k.apply(deployment("mesh", "web", 1, map[string]any{"name": "web", "image": pauseImage}))
	hosttest.Eventually(t, 5*time.Minute, "injected deployment ready", func() bool { return k.deploymentReady("mesh", "web") },
		func() string { return k.logs("mesh", "app=web") })

	pods := k.pods("mesh", "app=web")
	if len(pods) != 1 {
		t.Fatalf("%d web pods, want 1", len(pods))
	}
	p := pods[0]
	all := append(slices.Clone(p.Spec.InitContainers), p.Spec.Containers...)
	var names []string
	for _, c := range all {
		names = append(names, c.Name)
	}
	if !slices.Contains(names, "istio-init") {
		t.Errorf("containers %v: no istio-init (NET_ADMIN)", names)
	}
	want := map[string]string{"cpu": "500m", "memory": "2Gi", "ephemeral-storage": "1Gi"}
	for _, name := range []string{"istio-proxy", "web"} {
		i := slices.Index(names, name)
		if i < 0 {
			t.Errorf("containers %v: no %s", names, name)
			continue
		}
		for r, v := range want {
			if got := all[i].Resources.Requests[r]; got != v {
				t.Errorf("%s requests %s = %q, want Autopilot's default %s (all: %v)", name, r, got, v, all[i].Resources.Requests)
			}
		}
	}
}
