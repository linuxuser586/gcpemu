package gke

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
)

// compatNow is the clock time the check reports in unit tests.
var compatNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// rawPods decodes a JSON array of pods.
func rawPods(t *testing.T, s string) []json.RawMessage {
	t.Helper()
	var pods []json.RawMessage
	if err := json.Unmarshal([]byte(s), &pods); err != nil {
		t.Fatal(err)
	}
	return pods
}

// issue is a compatibility issue as the unit tests compare them: without
// lastObservation, description and documentationUrl.
type issue struct {
	constraint string
	typ        containerpb.AutopilotCompatibilityIssue_IssueType
	subjects   []string
}

// ownerMeta decodes an owner's metadata.
func ownerMeta(t *testing.T, s string) objectMeta {
	t.Helper()
	var m objectMeta
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func issuesOf(got []*containerpb.AutopilotCompatibilityIssue) []issue {
	var out []issue
	for _, i := range got {
		out = append(out, issue{i.ConstraintType, i.IncompatibilityType, i.Subjects})
	}
	return out
}

// TestCompatibilityIssues is FR-GKE-017's mapping of the Workload
// restrictions' results to issues.
func TestCompatibilityIssues(t *testing.T) {
	const withRequests = `"resources":{"requests":{"cpu":"500m","memory":"2Gi","ephemeral-storage":"1Gi"}}`
	for _, tc := range []struct {
		name   string
		pods   string
		owners map[string]objectMeta
		want   []issue
	}{
		{"privileged bare pod", `[{"metadata":{"name":"p","namespace":"default"},"status":{"phase":"Running"},
			"spec":{"containers":[{"name":"c",` + withRequests + `,"securityContext":{"privileged":true}}]}}]`, nil,
			[]issue{{"autogke-disallow-privilege", containerpb.AutopilotCompatibilityIssue_INCOMPATIBILITY, []string{"default/p"}}}},
		{"every workload passes", `[{"metadata":{"name":"p","namespace":"default"},"status":{"phase":"Running"},
			"spec":{"containers":[{"name":"c",` + withRequests + `}]}}]`, nil, nil},
		{"NET_ADMIN needs config", `[{"metadata":{"name":"p","namespace":"default"},"status":{"phase":"Running"},
			"spec":{"containers":[{"name":"c",` + withRequests + `,"securityContext":{"capabilities":{"add":["NET_ADMIN"]}}}]}}]`, nil,
			[]issue{{"autogke-default-linux-capabilities", containerpb.AutopilotCompatibilityIssue_ADDITIONAL_CONFIG_REQUIRED, []string{"default/p"}}}},
		{"SYS_ADMIN and NET_ADMIN", `[{"metadata":{"name":"p","namespace":"default"},"status":{"phase":"Running"},
			"spec":{"containers":[{"name":"c",` + withRequests + `,"securityContext":{"capabilities":{"add":["SYS_ADMIN","NET_ADMIN"]}}}]}}]`, nil,
			[]issue{
				{"autogke-default-linux-capabilities", containerpb.AutopilotCompatibilityIssue_INCOMPATIBILITY, []string{"default/p"}},
				{"autogke-default-linux-capabilities", containerpb.AutopilotCompatibilityIssue_ADDITIONAL_CONFIG_REQUIRED, []string{"default/p"}},
			}},
		{"requests counted against the Deployment", `[
			{"metadata":{"name":"web-5d8-a","namespace":"default","ownerReferences":[{"kind":"ReplicaSet","name":"web-5d8"}]},"status":{"phase":"Running"},"spec":{"containers":[{"name":"c"}]}},
			{"metadata":{"name":"web-5d8-b","namespace":"default","ownerReferences":[{"kind":"ReplicaSet","name":"web-5d8"}]},"status":{"phase":"Pending"},"spec":{"containers":[{"name":"c"}]}}]`,
			map[string]objectMeta{ownerKey("ReplicaSet", "default", "web-5d8"): ownerMeta(t, `{"name":"web-5d8","ownerReferences":[{"kind":"Deployment","name":"web"}]}`)},
			[]issue{{"autogke-pod-resources", containerpb.AutopilotCompatibilityIssue_PASSED_WITH_OPTIONAL_CONFIG, []string{"default/web"}}}},
		{"owners grouped and sorted", `[
			{"metadata":{"name":"z","namespace":"default"},"status":{"phase":"Running"},"spec":{"hostPID":true,"containers":[{"name":"c",` + withRequests + `}]}},
			{"metadata":{"name":"nightly-1-x","namespace":"ops","ownerReferences":[{"kind":"Job","name":"nightly-1"}]},"status":{"phase":"Running"},"spec":{"hostNetwork":true,"containers":[{"name":"c"}]}},
			{"metadata":{"name":"agent-x","namespace":"default","ownerReferences":[{"kind":"DaemonSet","name":"agent"}]},"status":{"phase":"Running"},"spec":{"hostNetwork":true,"containers":[{"name":"c",` + withRequests + `}]}},
			{"metadata":{"name":"agent-y","namespace":"default","ownerReferences":[{"kind":"DaemonSet","name":"agent"}]},"status":{"phase":"Running"},"spec":{"hostNetwork":true,"containers":[{"name":"c",` + withRequests + `}]}}]`,
			map[string]objectMeta{ownerKey("Job", "ops", "nightly-1"): ownerMeta(t, `{"name":"nightly-1","ownerReferences":[{"kind":"CronJob","name":"nightly"}]}`)},
			[]issue{
				{"autogke-disallow-hostnamespaces", containerpb.AutopilotCompatibilityIssue_INCOMPATIBILITY, []string{"default/agent", "default/z", "ops/nightly"}},
				{"autogke-pod-resources", containerpb.AutopilotCompatibilityIssue_PASSED_WITH_OPTIONAL_CONFIG, []string{"ops/nightly"}},
			}},
		{"kube-system and finished pods skipped", `[
			{"metadata":{"name":"s","namespace":"kube-system"},"status":{"phase":"Running"},"spec":{"containers":[{"name":"c","securityContext":{"privileged":true}}]}},
			{"metadata":{"name":"done","namespace":"default"},"status":{"phase":"Succeeded"},"spec":{"containers":[{"name":"c","securityContext":{"privileged":true}}]}},
			{"metadata":{"name":"crashed","namespace":"default"},"status":{"phase":"Failed"},"spec":{"containers":[{"name":"c","securityContext":{"privileged":true}}]}}]`, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := issuesOf(compatibilityIssues(rawPods(t, tc.pods), tc.owners, compatNow))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("issues = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestCompatibilityIssueFields is the fixed fields of FR-GKE-017's issues:
// the clock's time, a description per constraint and GKE's docs.
func TestCompatibilityIssueFields(t *testing.T) {
	got := compatibilityIssues(rawPods(t, `[{"metadata":{"name":"p","namespace":"default"},"status":{"phase":"Running"},
		"spec":{"containers":[{"name":"c","securityContext":{"privileged":true,"capabilities":{"add":["NET_ADMIN"]}}}]}}]`), nil, compatNow)
	if len(got) != 3 {
		t.Fatalf("issues = %v; want 3", got)
	}
	want := map[string]string{
		"autogke-default-linux-capabilities": "https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security",
		"autogke-disallow-privilege":         "https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security",
		"autogke-pod-resources":              "https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests",
	}
	for _, i := range got {
		if !i.LastObservation.AsTime().Equal(compatNow) || i.Description == "" || i.DocumentationUrl != want[i.ConstraintType] {
			t.Errorf("issue %v", i)
		}
	}
	if d := got[0].Description; !strings.Contains(d, "workloadPolicyConfig.allowNetAdmin") {
		t.Errorf("NET_ADMIN description = %q", d)
	}
}

// TestCheckAutopilotCompatibilityErrors is FR-GKE-017's refusals: the
// check runs only on a running Standard cluster.
func TestCheckAutopilotCompatibilityErrors(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	createRunning(t, a, apParent, autopilot("ap"))
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "std", InitialNodeCount: 1})
	check := func(name string) error {
		_, err := a.CheckAutopilotCompatibility(ctx, &containerpb.CheckAutopilotCompatibilityRequest{Name: apParent + "/clusters/" + name})
		return err
	}
	wantCode(t, "Autopilot cluster", check("ap"), codes.FailedPrecondition)
	wantCode(t, "unknown cluster", check("none"), codes.NotFound)
	r, _ := parseRef(apParent, "", "", "", "", 0)
	r.Cluster = "std"
	if err := a.s.updateCluster(r.key(), func(_ *clusterRecord, c *containerpb.Cluster) error {
		c.Status = containerpb.Cluster_PROVISIONING
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := check("std")
	wantCode(t, "provisioning cluster", err, codes.FailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), "not running") {
		t.Errorf("provisioning cluster: %v", err)
	}
}
