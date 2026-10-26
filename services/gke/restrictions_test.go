package gke

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// restrictedPodOf decodes a pod for the Workload restrictions.
func restrictedPodOf(t *testing.T, s string) *restrictedPod {
	t.Helper()
	var p restrictedPod
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// TestRestrictionViolations is FR-GKE-016's refusals, with GKE's constraint
// names and messages.
func TestRestrictionViolations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pod      string
		netAdmin bool
		want     map[string][]string
	}{
		{"plain pod", `{"spec":{"containers":[{"name":"c"}]}}`, false, nil},
		{"privileged", `{"spec":{"containers":[{"name":"nginx","securityContext":{"privileged":true}}]}}`, false,
			map[string][]string{"autogke-disallow-privilege": {"container nginx is privileged; not allowed in Autopilot"}}},
		{"host namespaces", `{"spec":{"hostNetwork":true,"hostPID":true,"hostIPC":true,"containers":[{"name":"c"}]}}`, false,
			map[string][]string{"autogke-disallow-hostnamespaces": {
				"enabling hostNetwork is not allowed in Autopilot.",
				"enabling hostPID is not allowed in Autopilot.",
				"enabling hostIPC is not allowed in Autopilot.",
			}}},
		{"host port", `{"spec":{"initContainers":[{"name":"i","ports":[{"containerPort":80,"hostPort":8080}]}],
			"containers":[{"name":"c","ports":[{"containerPort":443},{"containerPort":80,"hostPort":80}]}]}}`, false,
			map[string][]string{"autogke-no-host-port": {
				"container i specifies host port 8080; disallowed in Autopilot",
				"container c specifies host port 80; disallowed in Autopilot",
			}}},
		{"host path", `{"spec":{"volumes":[{"name":"root","hostPath":{"path":"/"}},{"name":"logs","hostPath":{"path":"/var/log/pods"}},{"name":"cfg","configMap":{"name":"x"}}],
			"containers":[{"name":"c","volumeMounts":[{"name":"root","mountPath":"/host"},{"name":"logs","mountPath":"/logs"},{"name":"cfg","mountPath":"/cfg"}]},
				{"name":"r","volumeMounts":[{"name":"logs","mountPath":"/logs","readOnly":true}]}]}}`, false,
			map[string][]string{"autogke-no-write-mode-hostpath": {
				"hostPath volume root used in container c uses path / which is not allowed in Autopilot. Allowed path prefixes for hostPath volumes are: [/var/log/].",
				"hostPath volume logs in container c is used in write mode; disallowed in Autopilot. Allowed read-only hostPath paths: [/var/log/].",
			}}},
		{"read-only log path", `{"spec":{"volumes":[{"name":"logs","hostPath":{"path":"/var/log"}}],
			"containers":[{"name":"c","volumeMounts":[{"name":"logs","mountPath":"/logs","readOnly":true}]}]}}`, false, nil},
		{"default capabilities", `{"spec":{"containers":[{"name":"c","securityContext":{"capabilities":{"add":["NET_BIND_SERVICE","CAP_NET_RAW","SYS_PTRACE"],"drop":["ALL"]}}}]}}`, false, nil},
		{"capability beyond the defaults", `{"spec":{"containers":[{"name":"c","securityContext":{"capabilities":{"add":["SYS_ADMIN","NET_ADMIN"]}}}]}}`, false,
			map[string][]string{"autogke-default-linux-capabilities": {
				"linux capability 'SYS_ADMIN' on container 'c' not allowed; Autopilot only allows the capabilities: 'AUDIT_WRITE,CHOWN,DAC_OVERRIDE,FOWNER,FSETID,KILL,MKNOD,NET_BIND_SERVICE,NET_RAW,SETFCAP,SETGID,SETPCAP,SETUID,SYS_CHROOT,SYS_PTRACE'.",
				"linux capability 'NET_ADMIN' on container 'c' not allowed; Autopilot only allows the capabilities: 'AUDIT_WRITE,CHOWN,DAC_OVERRIDE,FOWNER,FSETID,KILL,MKNOD,NET_BIND_SERVICE,NET_RAW,SETFCAP,SETGID,SETPCAP,SETUID,SYS_CHROOT,SYS_PTRACE'.",
			}}},
		{"NET_ADMIN allowed", `{"spec":{"initContainers":[{"name":"istio-init","securityContext":{"capabilities":{"add":["NET_ADMIN","NET_RAW"],"drop":["ALL"]}}}],"containers":[{"name":"c"}]}}`, true, nil},
		{"SYS_ADMIN with NET_ADMIN allowed", `{"spec":{"containers":[{"name":"c","securityContext":{"capabilities":{"add":["CAP_SYS_ADMIN"]}}}]}}`, true,
			map[string][]string{"autogke-default-linux-capabilities": {
				"linux capability 'CAP_SYS_ADMIN' on container 'c' not allowed; Autopilot only allows the capabilities: 'AUDIT_WRITE,CHOWN,DAC_OVERRIDE,FOWNER,FSETID,KILL,MKNOD,NET_ADMIN,NET_BIND_SERVICE,NET_RAW,SETFCAP,SETGID,SETPCAP,SETUID,SYS_CHROOT,SYS_PTRACE'.",
			}}},
		{"privileged init container", `{"spec":{"initContainers":[{"name":"i","securityContext":{"privileged":true}}],"containers":[{"name":"c"}]}}`, false,
			map[string][]string{"autogke-disallow-privilege": {"container i is privileged; not allowed in Autopilot"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := restrictionViolations(restrictedPodOf(t, tc.pod), tc.netAdmin)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("violations = %v; want %v", got, tc.want)
			}
		})
	}
}

// resourcePodOf decodes a pod for the request mutation.
func resourcePodOf(t *testing.T, s string) *resourcePod {
	t.Helper()
	var p resourcePod
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// TestAutopilotResources is FR-GKE-016's request mutation on clusters with
// bursting: defaults where a container sets none, the Pod's 50m CPU and
// 52 MiB memory minimums, and 1 to 6.5 GiB of memory per vCPU, counted
// over init and sidecar containers as the scheduler counts them. Values
// are GKE's documented rules worked by hand; no Recording of an Autopilot
// cluster exists yet (ADR 0023).
func TestAutopilotResources(t *testing.T) {
	type res = map[string]string
	for _, tc := range []struct {
		name     string
		pod      string
		requests map[string]res // container → requests after
		limits   map[string]res // container → limits after, when checked
		warning  string
	}{
		{"defaults", `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","resources":{}}]}}`,
			map[string]res{"c": {"cpu": "500m", "memory": "2Gi", "ephemeral-storage": "1Gi"}}, nil,
			"autopilot-default-resources-mutator:Autopilot updated Pod default/p: defaulted unspecified 'cpu' resource for containers [c], " +
				"defaulted unspecified 'ephemeral-storage' resource for containers [c], defaulted unspecified 'memory' resource for containers [c] " +
				"(see http://g.co/gke/autopilot-defaults)."},
		{"within the rules", `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","resources":{"requests":{"cpu":"500m","memory":"1Gi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"c": {"cpu": "500m", "memory": "1Gi", "ephemeral-storage": "1Gi"}}, nil, ""},
		{"minimums", `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","resources":{"requests":{"cpu":"10m","memory":"10Mi","ephemeral-storage":"1Gi"},"limits":{"cpu":"20m","memory":"100Mi"}}}]}}`,
			map[string]res{"c": {"cpu": "50m", "memory": "52Mi", "ephemeral-storage": "1Gi"}},
			map[string]res{"c": {"cpu": "50m", "memory": "100Mi"}},
			"autopilot-default-resources-mutator:Autopilot updated Pod default/p: adjusted resources to meet requirements for containers [c] (see http://g.co/gke/autopilot-resources)."},
		{"minimums over the pod", `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"a","resources":{"requests":{"cpu":"30m","memory":"40Mi","ephemeral-storage":"1Gi"}}},
			{"name":"b","resources":{"requests":{"cpu":"30m","memory":"40Mi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"a": {"cpu": "30m", "memory": "40Mi", "ephemeral-storage": "1Gi"}, "b": {"cpu": "30m", "memory": "40Mi", "ephemeral-storage": "1Gi"}}, nil, ""},
		{"memory raised", `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","resources":{"requests":{"cpu":"2","memory":"1Gi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"c": {"cpu": "2", "memory": "2Gi", "ephemeral-storage": "1Gi"}}, nil,
			"autopilot-default-resources-mutator:Autopilot updated Pod default/p: adjusted resources to meet requirements for containers [c] (see http://g.co/gke/autopilot-resources)."},
		{"CPU raised", `{"metadata":{"generateName":"web-"},"spec":{"containers":[{"name":"c","resources":{"requests":{"cpu":"250m","memory":"13Gi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"c": {"cpu": "2", "memory": "13Gi", "ephemeral-storage": "1Gi"}}, nil,
			"autopilot-default-resources-mutator:Autopilot updated Pod default/web-: adjusted resources to meet requirements for containers [c] (see http://g.co/gke/autopilot-resources)."},
		{"defaulted then raised", `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","resources":{"requests":{"cpu":"3"}}}]}}`,
			map[string]res{"c": {"cpu": "3", "memory": "3Gi", "ephemeral-storage": "1Gi"}}, nil,
			"autopilot-default-resources-mutator:Autopilot updated Pod default/p: defaulted unspecified 'ephemeral-storage' resource for containers [c], " +
				"defaulted unspecified 'memory' resource for containers [c]; adjusted resources to meet requirements for containers [c] (see http://g.co/gke/autopilot-defaults)."},
		{"init container counted", `{"metadata":{"name":"p"},"spec":{
			"initContainers":[{"name":"i","resources":{"requests":{"cpu":"1","memory":"2Gi"}}},{"name":"bare","resources":{}}],
			"containers":[{"name":"c","resources":{"requests":{"cpu":"1","memory":"512Mi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"i": {"cpu": "1", "memory": "2Gi"}, "c": {"cpu": "1", "memory": "512Mi", "ephemeral-storage": "1Gi"}}, nil, ""},
		{"init container dominating", `{"metadata":{"name":"p"},"spec":{
			"initContainers":[{"name":"i","resources":{"requests":{"cpu":"40m","memory":"3Gi"}}}],
			"containers":[{"name":"c","resources":{"requests":{"cpu":"10m","memory":"64Mi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"i": {"cpu": "40m", "memory": "3Gi"}, "c": {"cpu": "462m", "memory": "64Mi", "ephemeral-storage": "1Gi"}}, nil,
			"autopilot-default-resources-mutator:Autopilot updated Pod default/p: adjusted resources to meet requirements for containers [c] (see http://g.co/gke/autopilot-resources)."},
		{"sidecar counted", `{"metadata":{"name":"p"},"spec":{
			"initContainers":[{"name":"s","restartPolicy":"Always","resources":{"requests":{"cpu":"1","memory":"512Mi","ephemeral-storage":"1Gi"}}},
				{"name":"i","resources":{"requests":{"cpu":"1","memory":"1Gi"}}}],
			"containers":[{"name":"c","resources":{"requests":{"cpu":"1","memory":"1Gi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"s": {"cpu": "1", "memory": "512Mi", "ephemeral-storage": "1Gi"}, "i": {"cpu": "1", "memory": "1Gi"},
				"c": {"cpu": "1", "memory": "1536Mi", "ephemeral-storage": "1Gi"}}, nil,
			"autopilot-default-resources-mutator:Autopilot updated Pod default/p: adjusted resources to meet requirements for containers [c] (see http://g.co/gke/autopilot-resources)."},
		{"sidecar defaulted", `{"metadata":{"name":"p"},"spec":{
			"initContainers":[{"name":"s","restartPolicy":"Always","resources":{}}],
			"containers":[{"name":"c","resources":{"requests":{"cpu":"500m","memory":"1Gi","ephemeral-storage":"1Gi"}}}]}}`,
			map[string]res{"s": {"cpu": "500m", "memory": "2Gi", "ephemeral-storage": "1Gi"}}, nil,
			"autopilot-default-resources-mutator:Autopilot updated Pod default/p: defaulted unspecified 'cpu' resource for containers [s], " +
				"defaulted unspecified 'ephemeral-storage' resource for containers [s], defaulted unspecified 'memory' resource for containers [s] " +
				"(see http://g.co/gke/autopilot-defaults)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := resourcePodOf(t, tc.pod)
			ops, warning := autopilotResources(pod, "default")
			if warning != tc.warning {
				t.Errorf("warning:\n got %q\nwant %q", warning, tc.warning)
			}
			if (len(ops) == 0) != (tc.warning == "") {
				t.Errorf("%d patch ops with warning %q", len(ops), warning)
			}
			for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
				if want, ok := tc.requests[c.Name]; ok && !reflect.DeepEqual(c.Resources.Requests, want) {
					t.Errorf("container %s requests = %v; want %v", c.Name, c.Resources.Requests, want)
				}
				if want, ok := tc.limits[c.Name]; ok && !reflect.DeepEqual(c.Resources.Limits, want) {
					t.Errorf("container %s limits = %v; want %v", c.Name, c.Resources.Limits, want)
				}
			}
		})
	}
}

// admit sends one AdmissionReview to the cluster's Autopilot webhook hook.
func admit(t *testing.T, s *Service, key, hook string, req map[string]any) *admissionResponse {
	t.Helper()
	req["uid"] = "u1"
	b, _ := json.Marshal(map[string]any{"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview", "request": req})
	w := httptest.NewRecorder()
	s.serveAutopilotAdmission(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)), key, hook)
	var ar admissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &ar); err != nil || ar.Response == nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	if ar.Response.UID != "u1" {
		t.Errorf("uid %q", ar.Response.UID)
	}
	return ar.Response
}

// podReview is a pod CREATE by user.
func podReview(user, ns, pod string) map[string]any {
	return map[string]any{
		"kind": map[string]string{"group": "", "version": "v1", "kind": "Pod"}, "resource": map[string]string{"group": "", "version": "v1", "resource": "pods"},
		"namespace": ns, "operation": "CREATE", "object": json.RawMessage(pod),
		"userInfo": map[string]any{"username": user, "groups": []string{"system:authenticated"}},
	}
}

// TestAutopilotAdmission is FR-GKE-016 at the webhooks: an Autopilot
// cluster refuses what Autopilot refuses with GKE Warden's message,
// changes requests with GKE's warning and refuses user writes to
// kube-system but not the cluster's own (k3s's and the emulator's); a Standard
// cluster admits all of it unchanged.
func TestAutopilotAdmission(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	createRunning(t, a, apParent, autopilot("ap"))
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "std", InitialNodeCount: 1})
	ap, std := clusterKey("ap-test", "us-central1", "ap"), clusterKey("ap-test", "us-central1", "std")
	const user = "dev@example.com"

	privileged := `{"metadata":{"name":"nginx"},"spec":{"containers":[{"name":"nginx","securityContext":{"privileged":true},"resources":{}}]}}`
	r := admit(t, a.s, ap, hookRestrict, podReview(user, "default", privileged))
	want := "GKE Warden rejected the request because it violates one or more constraints.\n" +
		`Violations details: {"[denied by autogke-disallow-privilege]":["container nginx is privileged; not allowed in Autopilot"]}` + "\n" +
		"Requested by user: 'dev@example.com', groups: 'system:authenticated'."
	if r.Allowed || r.Status == nil || r.Status.Message != want || r.Status.Code != http.StatusForbidden || r.Status.Reason != "GKE Warden constraints violations" {
		t.Errorf("privileged pod on Autopilot: %+v %+v; want message\n%s", r, r.Status, want)
	}
	if r := admit(t, a.s, std, hookRestrict, podReview(user, "default", privileged)); !r.Allowed {
		t.Errorf("privileged pod refused on Standard: %+v", r.Status)
	}

	// A workload whose pod template Autopilot would refuse is refused
	// itself, with the same message.
	for _, w := range []struct{ group, resource, object string }{
		{"apps", "deployments", `{"spec":{"template":{"spec":{"containers":[{"name":"nginx","securityContext":{"privileged":true}}]}}}}`},
		{"batch", "cronjobs", `{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"nginx","securityContext":{"privileged":true}}]}}}}}}`},
	} {
		req := podReview(user, "default", w.object)
		req["resource"] = map[string]string{"group": w.group, "version": "v1", "resource": w.resource}
		req["operation"] = "UPDATE"
		if r := admit(t, a.s, ap, hookRestrict, req); r.Allowed || r.Status == nil || !strings.Contains(r.Status.Message, "container nginx is privileged") {
			t.Errorf("privileged %s on Autopilot: %+v", w.resource, r.Status)
		}
		if r := admit(t, a.s, std, hookRestrict, req); !r.Allowed {
			t.Errorf("privileged %s refused on Standard", w.resource)
		}
	}

	// Requests are changed, with GKE's warning, on Autopilot only.
	r = admit(t, a.s, ap, hookResources, podReview(user, "default", privileged))
	if !r.Allowed || r.PatchType != "JSONPatch" || r.Patch == "" || len(r.Warnings) != 1 ||
		!strings.HasPrefix(r.Warnings[0], "autopilot-default-resources-mutator:Autopilot updated Pod default/nginx: defaulted unspecified 'cpu'") {
		t.Errorf("bare pod on Autopilot: %+v", r)
	}
	if r := admit(t, a.s, std, hookResources, podReview(user, "default", privileged)); !r.Allowed || r.Patch != "" || len(r.Warnings) != 0 {
		t.Errorf("bare pod on Standard: %+v", r)
	}

	// NET_ADMIN is refused until allowNetAdmin is set.
	netAdmin := `{"metadata":{"name":"p"},"spec":{"initContainers":[{"name":"istio-init","securityContext":{"capabilities":{"add":["NET_ADMIN","NET_RAW"]}}}],"containers":[{"name":"c"}]}}`
	if r := admit(t, a.s, ap, hookRestrict, podReview(user, "default", netAdmin)); r.Allowed {
		t.Error("NET_ADMIN admitted without allowNetAdmin")
	}
	yes := true
	if _, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: apParent + "/clusters/ap", Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AllowNetAdmin: &yes}}}); err != nil {
		t.Fatal(err)
	}
	if r := admit(t, a.s, ap, hookRestrict, podReview(user, "default", netAdmin)); !r.Allowed {
		t.Errorf("NET_ADMIN refused with allowNetAdmin: %+v", r.Status)
	}

	// Writes to kube-system.
	cm := `{"metadata":{"name":"x"}}`
	write := func(user, op string) map[string]any {
		return map[string]any{
			"kind": map[string]string{"version": "v1", "kind": "ConfigMap"}, "resource": map[string]string{"version": "v1", "resource": "configmaps"},
			"namespace": "kube-system", "name": "x", "operation": op, "object": json.RawMessage(cm),
			"userInfo": map[string]any{"username": user, "groups": []string{"system:authenticated"}},
		}
	}
	for _, tc := range []struct {
		user, op string
		allowed  bool
	}{
		{user, "CREATE", false},
		{user, "UPDATE", false},
		{"system:serviceaccount:default:app", "UPDATE", false},
		{"system:admin", "CREATE", true},
		{"system:serviceaccount:kube-system:helm", "CREATE", true},
		{"system:kube-controller-manager", "UPDATE", true},
	} {
		r := admit(t, a.s, ap, hookRestrict, write(tc.user, tc.op))
		if r.Allowed != tc.allowed {
			t.Errorf("%s in kube-system by %s: allowed %v; want %v", tc.op, tc.user, r.Allowed, tc.allowed)
		}
	}
	r = admit(t, a.s, ap, hookRestrict, write(user, "UPDATE"))
	if want := `{"[denied by managed-namespaces-limitation]":["the namespace \"kube-system\" is managed and the request's verb \"update\" is denied"]}`; r.Status == nil || !strings.Contains(r.Status.Message, want) {
		t.Errorf("kube-system update message: %+v; want it to contain %s", r.Status, want)
	}
	if r := admit(t, a.s, std, hookRestrict, write(user, "CREATE")); !r.Allowed {
		t.Error("kube-system write refused on Standard")
	}
}
