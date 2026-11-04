package hosttest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
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
					{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/1 nodes are available"},
					{"type":"Ready","status":"False","reason":"ContainersNotReady","message":"containers with unready status: [web]"}],
				"initContainerStatuses":[{"name":"setup","restartCount":3,
					"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 40s"}},
					"lastState":{"terminated":{"reason":"Error","exitCode":2}}}],
				"containerStatuses":[{"name":"web","state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}]}`,
		"/api/v1/nodes": `{"items":[
			{"metadata":{"name":"node-a"},"status":{"conditions":[
				{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"}]}},
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
	k := Kube{Endpoint: srv.URL, Client: srv.Client()}

	got := k.Diagnose("default", "app=web")
	for _, want := range []string{
		"web-1 phase=Pending node=node-a\n",
		"PodScheduled=False Unschedulable: 0/1 nodes are available\n",
		"Ready=False ContainersNotReady: containers with unready status: [web]\n",
		"setup restarts=3 waiting CrashLoopBackOff: back-off 40s, last terminated Error exit 2\n",
		"web restarts=0 waiting ImagePullBackOff: Back-off pulling image\n",
		"node-a Ready=True DiskPressure=False\n",
		"node-b Ready=False KubeletNotReady DiskPressure=True ephemeral storage low taints=[node.kubernetes.io/disk-pressure:NoSchedule]\n",
		"23:12:01 Normal Pulling Pod/web-1: Pulling image\n  23:12:30 Warning Failed Pod/web-1 x4: rate limited\n",
		"--- web-1\nlog: 502 \n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Diagnose is missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Initialized") || strings.Contains(got, "MemoryPressure") {
		t.Errorf("Diagnose prints healthy conditions:\n%s", got)
	}
}

// TestDiagnoseReportsAnUnreadableAPI keeps an API failure from reading as a
// cluster with no pods.
func TestDiagnoseReportsAnUnreadableAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "control plane down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	k := Kube{Endpoint: srv.URL, Client: srv.Client()}

	got := k.Diagnose("default", "app=web")
	for _, want := range []string{"pods: 503 control plane down", "nodes: 503 control plane down", "events: 503 control plane down"} {
		if !strings.Contains(got, want) {
			t.Errorf("Diagnose is missing %q in:\n%s", want, got)
		}
	}
}

// TestDiagnoseReportsAnUndecodableAPI keeps a 200 that is not the API's, such
// as a proxy's page, from reading as a cluster with no pods.
func TestDiagnoseReportsAnUndecodableAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not the API</html>"))
	}))
	defer srv.Close()

	got := Kube{Endpoint: srv.URL, Client: srv.Client()}.Diagnose("default", "app=web")
	for _, want := range []string{"pods: 0 undecodable 200: <html>not the API</html>", "nodes: 0 undecodable 200: ", "events: 0 undecodable 200: "} {
		if !strings.Contains(got, want) {
			t.Errorf("Diagnose is missing %q in:\n%s", want, got)
		}
	}
}

// TestDiagnosePodChoosesThePodByName serves only the pod named in the list
// query, for a pod with no labels to select.
func TestDiagnosePodChoosesThePodByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/namespaces/default/pods" && r.URL.Query().Get("fieldSelector") == "metadata.name=dns":
			w.Write([]byte(`{"items":[{"metadata":{"name":"dns"},"status":{"phase":"Pending"}}]}`))
		case r.URL.Path == "/api/v1/namespaces/default/pods":
			w.Write([]byte(`{"items":[{"metadata":{"name":"other"},"status":{"phase":"Running"}}]}`))
		case r.URL.Path == "/api/v1/namespaces/default/pods/dns/log":
			http.Error(w, "container is waiting to start", http.StatusBadRequest)
		default:
			w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer srv.Close()

	got := Kube{Endpoint: srv.URL, Client: srv.Client()}.DiagnosePod("default", "dns")
	for _, want := range []string{"dns phase=Pending", "--- dns\nlog: 400 container is waiting to start\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("DiagnosePod is missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "other") {
		t.Errorf("DiagnosePod reports another pod:\n%s", got)
	}
}

// fatalTB records Fatalf and ends the goroutine, as a test's Fatalf does.
type fatalTB struct {
	testing.TB
	msg string
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// TestTimeoutSurvivesAnUnreachableAPI times out with a diagnosis of an API
// server that is gone: the report says so, and the timeout still reports.
func TestTimeoutSurvivesAnUnreachableAPI(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	k := Kube{Endpoint: srv.URL, Client: srv.Client()}
	srv.Close()

	tb := &fatalTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Eventually(tb, 0, "a pod", func() bool { time.Sleep(time.Millisecond); return false }, func() string { return k.Diagnose("default", "app=web") })
	}()
	<-done
	for _, want := range []string{"timed out after 0s waiting for a pod\n", "pods: 0 ", "nodes: 0 ", "events: 0 "} {
		if !strings.Contains(tb.msg, want) {
			t.Errorf("timeout is missing %q in:\n%s", want, tb.msg)
		}
	}
}
