//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDiagnoseSaysWhyPodsAreNotReady serves the API responses of a pod that
// cannot start, whose log request fails with an empty body (#966).
func TestDiagnoseSaysWhyPodsAreNotReady(t *testing.T) {
	bodies := map[string]string{
		"/api/v1/namespaces/default/pods": `{"items":[{
			"metadata":{"name":"web-1"},
			"spec":{"nodeName":"node-a"},
			"status":{"phase":"Pending",
				"conditions":[
					{"type":"Initialized","status":"True"},
					{"type":"Ready","status":"False","reason":"ContainersNotReady","message":"containers with unready status: [web]"}],
				"initContainerStatuses":[{"name":"setup","restartCount":3,
					"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 40s"}},
					"lastState":{"terminated":{"reason":"Error","exitCode":2}}}],
				"containerStatuses":[{"name":"web","state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}]}`,
		"/api/v1/nodes": `{"items":[
			{"metadata":{"name":"node-a"},"status":{"conditions":[
				{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"}]}},
			{"metadata":{"name":"node-b"},
				"spec":{"taints":[{"key":"node.kubernetes.io/disk-pressure","effect":"NoSchedule"}]},
				"status":{"conditions":[{"type":"Ready","status":"False","reason":"KubeletNotReady"},
					{"type":"DiskPressure","status":"True","message":"ephemeral storage low"}]}}]}`,
		"/api/v1/namespaces/default/events": `{"items":[
			{"type":"Warning","reason":"Failed","message":"rate limited","count":4,
				"lastTimestamp":"2026-11-03T23:12:30Z","involvedObject":{"kind":"Pod","name":"web-1"}},
			{"type":"Normal","reason":"Pulling","message":"Pulling image",
				"eventTime":"2026-11-03T23:12:01.000000Z","involvedObject":{"kind":"Pod","name":"web-1"}}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := bodies[r.URL.Path]; ok {
			w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	k := &kube{t: t, endpoint: srv.URL, hc: srv.Client()}

	got := k.diagnose("default", "app=web")
	for _, want := range []string{
		"web-1 phase=Pending node=node-a\n",
		"Ready=False ContainersNotReady: containers with unready status: [web]\n",
		"setup restarts=3 waiting CrashLoopBackOff: back-off 40s, last terminated Error exit 2\n",
		"web restarts=0 waiting ImagePullBackOff: Back-off pulling image\n",
		"node-a Ready=True\n",
		"node-b Ready=False KubeletNotReady DiskPressure=True ephemeral storage low taints=[node.kubernetes.io/disk-pressure:NoSchedule]\n",
		"23:12:01 Normal Pulling Pod/web-1: Pulling image\n  23:12:30 Warning Failed Pod/web-1 x4: rate limited\n",
		"--- web-1\nlog: 502 \n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnose is missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Initialized") || strings.Contains(got, "MemoryPressure") {
		t.Errorf("diagnose prints healthy conditions:\n%s", got)
	}
}

// TestDiagnoseReportsAnUnreadableAPI keeps an API failure from reading as a
// cluster with no pods.
func TestDiagnoseReportsAnUnreadableAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "control plane down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	k := &kube{t: t, endpoint: srv.URL, hc: srv.Client()}

	got := k.diagnose("default", "app=web")
	for _, want := range []string{"pods: 503 control plane down", "nodes: 503 control plane down", "events: 503 control plane down"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnose is missing %q in:\n%s", want, got)
		}
	}
}
