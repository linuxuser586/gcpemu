package gke

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestScalePodRequests is the size FR-GKE-015's node provisioning gives a
// pending Pod: its CPU and memory as the scheduler counts them, as the
// Workload restrictions count them (FR-GKE-016), plus its overhead. A
// sidecar started before an init container runs alongside it; one started
// after does not.
func TestScalePodRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want res
	}{
		{"containers", `{"containers":[{"resources":{"requests":{"cpu":"200m","memory":"128Mi"}}},{"resources":{"requests":{"cpu":"300m","memory":"64Mi"}}}]}`,
			res{cpu: 500, mem: 192 << 20, pods: 1}},
		{"no requests", `{"containers":[{"resources":{}}]}`, res{pods: 1}},
		{"init container larger", `{"initContainers":[{"resources":{"requests":{"cpu":"1","memory":"1Gi"}}}],
			"containers":[{"resources":{"requests":{"cpu":"200m","memory":"128Mi"}}}]}`,
			res{cpu: 1000, mem: 1 << 30, pods: 1}},
		{"sidecar before init container", `{"initContainers":[
				{"restartPolicy":"Always","resources":{"requests":{"cpu":"500m","memory":"256Mi"}}},
				{"resources":{"requests":{"cpu":"1","memory":"1Gi"}}}],
			"containers":[{"resources":{"requests":{"cpu":"200m","memory":"128Mi"}}}]}`,
			res{cpu: 1500, mem: 1280 << 20, pods: 1}},
		{"sidecar after init container", `{"initContainers":[
				{"resources":{"requests":{"cpu":"1","memory":"1Gi"}}},
				{"restartPolicy":"Always","resources":{"requests":{"cpu":"500m","memory":"256Mi"}}}],
			"containers":[{"resources":{"requests":{"cpu":"200m","memory":"128Mi"}}}]}`,
			res{cpu: 1000, mem: 1 << 30, pods: 1}},
		{"sidecars outgrowing init container", `{"initContainers":[
				{"resources":{"requests":{"cpu":"1","memory":"1Gi"}}},
				{"restartPolicy":"Always","resources":{"requests":{"cpu":"900m","memory":"1Gi"}}}],
			"containers":[{"resources":{"requests":{"cpu":"200m","memory":"128Mi"}}}]}`,
			res{cpu: 1100, mem: 1152 << 20, pods: 1}},
		{"sidecars between init containers", `{"initContainers":[
				{"restartPolicy":"Always","resources":{"requests":{"cpu":"100m","memory":"64Mi"}}},
				{"resources":{"requests":{"cpu":"1","memory":"512Mi"}}},
				{"restartPolicy":"Always","resources":{"requests":{"cpu":"200m","memory":"128Mi"}}},
				{"resources":{"requests":{"cpu":"800m","memory":"1Gi"}}}],
			"containers":[{"resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}`,
			res{cpu: 1100, mem: 1216 << 20, pods: 1}},
		{"overhead", `{"initContainers":[
				{"restartPolicy":"Always","resources":{"requests":{"cpu":"500m","memory":"256Mi"}}},
				{"resources":{"requests":{"cpu":"1","memory":"1Gi"}}}],
			"containers":[{"resources":{"requests":{"cpu":"200m","memory":"128Mi"}}}],
			"overhead":{"cpu":"250m","memory":"120Mi"}}`,
			res{cpu: 1750, mem: 1400 << 20, pods: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p scalePod
			if err := json.Unmarshal([]byte(`{"spec":`+tc.spec+`}`), &p); err != nil {
				t.Fatal(err)
			}
			if got := p.requests(); got != tc.want {
				t.Errorf("requests() = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestNewNodeLabels is FR-GKE-015: the node Autopilot plans for a Pending
// pod gets the compute class and Spot the pod asks for through its node
// selector or a term of its required node affinity, and is planned only
// when the pod would schedule onto it, so that no node is added in a loop.
func TestNewNodeLabels(t *testing.T) {
	const cc, spot = computeClassLabel, spotLabel
	affinity := func(terms ...string) string {
		return `"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[` + strings.Join(terms, ",") + `]}}}`
	}
	term := func(exprs ...string) string { return `{"matchExpressions":[` + strings.Join(exprs, ",") + `]}` }
	expr := func(key, op string, values ...string) string {
		v, _ := json.Marshal(values)
		return `{"key":"` + key + `","operator":"` + op + `","values":` + string(v) + `}`
	}
	tests := []struct {
		name, spec string
		// class and spot are the labels of the planned node; ok is
		// whether one is planned.
		class, spot string
		ok          bool
	}{
		{"none", `{}`, "", "", true},
		{"selector", `{"nodeSelector":{"` + cc + `":"Scale-Out","` + spot + `":"true"}}`, "Scale-Out", "true", true},
		{"affinity compute class", `{` + affinity(term(expr(cc, "In", "Scale-Out"))) + `}`, "Scale-Out", "", true},
		{"affinity Spot", `{` + affinity(term(expr(spot, "In", "true"))) + `}`, "", "true", true},
		{"affinity both", `{` + affinity(term(expr(cc, "In", "Balanced"), expr(spot, "In", "true"))) + `}`, "Balanced", "true", true},
		{"several values", `{` + affinity(term(expr(cc, "In", "Scale-Out", "Balanced"))) + `}`, "Scale-Out", "", true},
		{"several terms", `{` + affinity(term(expr(cc, "In", "Scale-Out")), term(expr(spot, "In", "true"))) + `}`, "Scale-Out", "", true},
		{"selector with affinity", `{"nodeSelector":{"` + cc + `":"Scale-Out"},` + affinity(term(expr(spot, "In", "true"))) + `}`, "Scale-Out", "true", true},
		{"NotIn", `{` + affinity(term(expr(cc, "NotIn", "Scale-Out"))) + `}`, "", "", true},
		{"Spot NotIn", `{` + affinity(term(expr(spot, "NotIn", "true"))) + `}`, "", "", true},
		{"Spot false", `{` + affinity(term(expr(spot, "In", "false"))) + `}`, "", "", false},
		{"selector and affinity disagree", `{"nodeSelector":{"` + cc + `":"Balanced"},` + affinity(term(expr(cc, "In", "Scale-Out"))) + `}`, "", "", false},
		{"Exists", `{` + affinity(term(expr(cc, "Exists"))) + `}`, "", "", false},
		{"empty term", `{` + affinity(term()) + `}`, "", "", false},
	}
	c := &containerpb.Cluster{Id: "c1", Autopilot: &containerpb.Autopilot{Enabled: true}}
	np := &containerpb.NodePool{Name: defaultPoolName, Locations: []string{"us-central1-a"}, Config: &containerpb.NodeConfig{}}
	a := &autoscaler{s: &Service{env: &emu.Env{IDs: emu.NewIDs(true)}}}
	capOf := func(nodeRecord) res { return res{cpu: 4000, mem: 16 << 30, pods: 32} }
	labelsOf := func(n nodeRecord) map[string]string {
		m := map[string]string{}
		for _, l := range nodeLabels(c, np, n) {
			k, v, _ := strings.Cut(l, "=")
			m[k] = v
		}
		return m
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p scalePod
			if err := json.Unmarshal([]byte(`{"spec":`+tt.spec+`}`), &p); err != nil {
				t.Fatal(err)
			}
			n, zones := a.newNode(c, np, &p, res{cpu: 100, mem: 64 << 20, pods: 1}, map[string]int{}, capOf, labelsOf)
			if ok := len(zones) > 0; ok != tt.ok {
				t.Fatalf("newNode planned a node: %v; want %v", ok, tt.ok)
			}
			if !tt.ok {
				return
			}
			l := labelsOf(n)
			if l[cc] != tt.class || l[spot] != tt.spot {
				t.Errorf("node labels %s=%q, %s=%q; want %q, %q", cc, l[cc], spot, l[spot], tt.class, tt.spot)
			}
			if prov := l["cloud.google.com/gke-provisioning"]; (prov == "spot") != (tt.spot == "true") {
				t.Errorf("gke-provisioning=%q with Spot %q", prov, tt.spot)
			}
		})
	}
}

// TestNewNodeZone is FR-GKE-015: the node Autopilot plans for a Pending
// pod is in a zone its node selector and required node affinity accept,
// the one with fewest nodes, and zones are those it could be planned in,
// which FailedScaleUp names. A pod accepting none of the cluster's zones
// gets no node.
func TestNewNodeZone(t *testing.T) {
	const zl, cc = "topology.kubernetes.io/zone", computeClassLabel
	affinity := func(terms ...string) string {
		return `"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[` + strings.Join(terms, ",") + `]}}}`
	}
	term := func(exprs ...string) string { return `{"matchExpressions":[` + strings.Join(exprs, ",") + `]}` }
	expr := func(key, op string, values ...string) string {
		v, _ := json.Marshal(values)
		return `{"key":"` + key + `","operator":"` + op + `","values":` + string(v) + `}`
	}
	const za, zb, zc = "us-central1-a", "us-central1-b", "us-central1-c"
	tests := []struct {
		name, spec string
		// zone is the planned node's zone, and zones the zones a node
		// could be planned in; none when zones is empty.
		zone  string
		zones []string
	}{
		{"none", `{}`, za, []string{za, zb, zc}},
		{"selector", `{"nodeSelector":{"` + zl + `":"` + zc + `"}}`, zc, []string{zc}},
		{"affinity", `{` + affinity(term(expr(zl, "In", zc))) + `}`, zc, []string{zc}},
		{"affinity several zones", `{` + affinity(term(expr(zl, "In", zc, zb))) + `}`, zb, []string{zb, zc}},
		{"affinity NotIn", `{` + affinity(term(expr(zl, "NotIn", za))) + `}`, zb, []string{zb, zc}},
		{"affinity several terms", `{` + affinity(term(expr(zl, "In", zc)), term(expr(zl, "In", zb), expr(cc, "In", "Scale-Out"))) + `}`, zb, []string{zb, zc}},
		{"zone outside the cluster", `{` + affinity(term(expr(zl, "In", "us-east1-b"))) + `}`, "", nil},
		{"selector outside the cluster", `{"nodeSelector":{"` + zl + `":"us-east1-b"}}`, "", nil},
		{"selector and affinity disagree", `{"nodeSelector":{"` + zl + `":"` + za + `"},` + affinity(term(expr(zl, "In", zc))) + `}`, "", nil},
	}
	c := &containerpb.Cluster{Id: "c1", Autopilot: &containerpb.Autopilot{Enabled: true}}
	np := &containerpb.NodePool{Name: defaultPoolName, Locations: []string{za, zb, zc}, Config: &containerpb.NodeConfig{}}
	a := &autoscaler{s: &Service{env: &emu.Env{IDs: emu.NewIDs(true)}}}
	capOf := func(nodeRecord) res { return res{cpu: 4000, mem: 16 << 30, pods: 32} }
	labelsOf := func(n nodeRecord) map[string]string {
		m := map[string]string{}
		for _, l := range nodeLabels(c, np, n) {
			k, v, _ := strings.Cut(l, "=")
			m[k] = v
		}
		return m
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p scalePod
			if err := json.Unmarshal([]byte(`{"spec":`+tt.spec+`}`), &p); err != nil {
				t.Fatal(err)
			}
			perZone := map[string]int{za: 1, zb: 2, zc: 3}
			n, zones := a.newNode(c, np, &p, res{cpu: 100, mem: 64 << 20, pods: 1}, perZone, capOf, labelsOf)
			if !slices.Equal(zones, tt.zones) {
				t.Fatalf("zones %v; want %v", zones, tt.zones)
			}
			if len(zones) > 0 && n.Zone != tt.zone {
				t.Errorf("node in %s; want %s", n.Zone, tt.zone)
			}
		})
	}
}
