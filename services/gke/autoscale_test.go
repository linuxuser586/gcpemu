package gke

import (
	"encoding/json"
	"testing"
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
