package gke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePodAPI serves a pod with ip once listed misses times, as an API
// server does before the kubelet reports a new pod's IP (#134).
func fakePodAPI(t *testing.T, ip string, misses int32) (*Service, *atomic.Int32) {
	t.Helper()
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/pods":
			if r.URL.Query().Get("fieldSelector") != "status.podIP="+ip || lists.Add(1) <= misses {
				_, _ = w.Write([]byte(`{"items":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"job-x","namespace":"ns"},"spec":{"serviceAccountName":"ksa"},"status":{"phase":"Running","podIP":"` + ip + `"}}]}`))
		case r.URL.Path == "/api/v1/namespaces/ns/serviceaccounts/ksa":
			_, _ = w.Write([]byte(`{"metadata":{"name":"ksa","annotations":{"iam.gke.io/gcp-service-account":"app@p.iam.gserviceaccount.com"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s := &Service{clusters: map[string]*clusterRT{"k": {kube: &kubeClient{base: srv.URL, hc: srv.Client()}}}}
	return s, &lists
}

func setPodWait(t *testing.T, wait, poll time.Duration) {
	t.Helper()
	ow, op := podWait, podPoll
	podWait, podPoll = wait, poll
	t.Cleanup(func() { podWait, podPoll = ow, op })
}

// TestResolvePodWaitsForNewPod is #134: a request from a pod the API
// server does not know yet waits for it instead of answering 404.
func TestResolvePodWaitsForNewPod(t *testing.T) {
	setPodWait(t, 5*time.Second, 10*time.Millisecond)
	s, lists := fakePodAPI(t, "10.4.1.28", 3)
	e := s.resolvePod(context.Background(), "k", "10.4.1.28")
	if e.err != nil || e.ns != "ns" || e.ksa != "ksa" || e.gsa != "app@p.iam.gserviceaccount.com" {
		t.Fatalf("resolvePod = %+v", e)
	}
	if n := lists.Load(); n != 4 {
		t.Errorf("pods listed %d times, want 4", n)
	}
	// Found pods are cached.
	if e := s.resolvePod(context.Background(), "k", "10.4.1.28"); e.err != nil || lists.Load() != 4 {
		t.Errorf("cached resolvePod = %+v after %d lists", e, lists.Load())
	}
}

// TestResolvePodGivesUp: an IP that no pod has is a 404 after podWait,
// or as soon as the caller goes away.
func TestResolvePodGivesUp(t *testing.T) {
	setPodWait(t, 200*time.Millisecond, 10*time.Millisecond)
	s, _ := fakePodAPI(t, "10.4.1.28", 0)
	start := time.Now()
	e := s.resolvePod(context.Background(), "k", "10.4.1.29")
	if e.err == nil || !strings.Contains(e.err.Error(), "no pod has IP 10.4.1.29") {
		t.Fatalf("resolvePod = %+v", e)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Errorf("gave up after %v, want about 200ms", d)
	}

	setPodWait(t, time.Minute, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if e := s.resolvePod(ctx, "k", "10.4.1.29"); e.err == nil {
		t.Fatalf("resolvePod = %+v", e)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("caller gone, still waited %v", d)
	}
}
