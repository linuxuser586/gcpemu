package gke

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

type tokenAuthn map[string]emu.Principal

func (a tokenAuthn) Authenticate(_ context.Context, tok string) (emu.Principal, bool) {
	p, ok := a[tok]
	return p, ok
}

func lanTestService(mode string, gkeBind ...string) *Service {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Defaults()
	cfg.GKEBind = gkeBind
	auth := emu.NewPolicyAuthorizer(mode, log)
	auth.Install(tokenAuthn{"good": "user:alice@example.com"}, nil)
	env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: auth, Endpoints: emu.NewEndpoints()}
	return New(env).(*Service)
}

func reviewToken(t *testing.T, s *Service, tok string) (bool, string) {
	t.Helper()
	body := `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"` + tok + `"}}`
	w := httptest.NewRecorder()
	s.serveTokenReview(w, httptest.NewRequest("POST", "/", strings.NewReader(body)), "k")
	var tr tokenReview
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Status.User == nil {
		return tr.Status.Authenticated, ""
	}
	return tr.Status.Authenticated, tr.Status.User.Username
}

// With --gke-bind the API server is reachable from the LAN, so only real
// credentials authenticate, whatever the IAM enforcement level.
func TestTokenReviewStrictWithGKEBind(t *testing.T) {
	for _, mode := range []string{config.IAMOff, config.IAMAudit, config.IAMEnforce} {
		t.Run(mode, func(t *testing.T) {
			loop := lanTestService(mode)
			lan := lanTestService(mode, "192.168.1.20")
			for _, s := range []*Service{loop, lan} {
				if ok, user := reviewToken(t, s, "good"); !ok || user != "alice@example.com" {
					t.Errorf("good token = %v %q", ok, user)
				}
			}
			ok, user := reviewToken(t, loop, "bogus")
			if want := mode != config.IAMEnforce; ok != want || (ok && user != "dev@example.com") {
				t.Errorf("loopback bogus token = %v %q, want %v", ok, user, want)
			}
			if ok, _ := reviewToken(t, lan, "bogus"); ok {
				t.Error("--gke-bind: bogus token authenticated")
			}
		})
	}
}

func TestLANAddress(t *testing.T) {
	s := lanTestService(config.IAMAudit, "192.168.1.20", "10.0.0.5")
	for v, want := range map[string]string{"": "", "false": "", "true": "192.168.1.20", "1": "192.168.1.20", "10.0.0.5": "10.0.0.5"} {
		if got, err := s.lanAddress(v); err != nil || got != want {
			t.Errorf("lanAddress(%q) = %q, %v; want %q", v, got, err, want)
		}
	}
	if _, err := s.lanAddress("10.9.9.9"); err == nil {
		t.Error("lanAddress accepted an address that is not --gke-bind")
	}
	if _, err := lanTestService(config.IAMAudit).lanAddress("true"); err == nil || !strings.Contains(err.Error(), "--gke-bind") {
		t.Errorf("lan=true without --gke-bind: %v", err)
	}
}

// putRunningCluster stores a running cluster proj-1/us-central1/c whose API
// server is at loopback, as the host's clients reach it, and served on the
// --gke-bind addresses lan.
func putRunningCluster(t *testing.T, s *Service, loopback string, lan ...string) {
	t.Helper()
	rec := &clusterRecord{Int: clusterInternal{Project: "proj-1", Location: "us-central1", Name: "c"}}
	t1 := true
	rec.setCluster(&containerpb.Cluster{
		Name: "c", Location: "us-central1", Status: containerpb.Cluster_RUNNING, Endpoint: loopback,
		ControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{IpEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{
			Enabled: &t1, EnablePublicEndpoint: &t1, PublicEndpoint: loopback, PrivateEndpoint: loopback,
		}},
	})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	rt := s.rtFor(rec.key())
	rt.mu.Lock()
	rt.lanAddrs = lan
	rt.mu.Unlock()
}

// A caller that arrived through the LAN, over a link or the Google front
// end, is told the cluster's --gke-bind address, the one it arrived at
// when the cluster is served there; one on loopback, the loopback address.
func TestClusterEndpointByCaller(t *testing.T) {
	s := lanTestService(config.IAMOff, "192.168.1.20", "10.0.0.5")
	s.env.Config.Projects = []string{"proj-1"}
	putRunningCluster(t, s, "127.0.0.1:41000", "192.168.1.20:42000", "10.0.0.5:42000")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"loopback", context.Background(), "127.0.0.1:41000"},
		{"arrived at a --gke-bind address", emu.WithLANCaller(context.Background(), "10.0.0.5"), "10.0.0.5:42000"},
		{"arrived elsewhere", emu.WithLANCaller(context.Background(), "10.9.9.9"), "192.168.1.20:42000"},
	} {
		ctx, want := tc.ctx, tc.want
		c, err := s.api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: "projects/proj-1/locations/us-central1/clusters/c"})
		if err != nil {
			t.Fatal(err)
		}
		ipc := c.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig()
		if c.Endpoint != want || ipc.GetPublicEndpoint() != want || ipc.GetPrivateEndpoint() != want || c.StatusMessage != "" {
			t.Errorf("%s: endpoints = %q %q %q (%q), want %q", tc.name, c.Endpoint, ipc.GetPublicEndpoint(), ipc.GetPrivateEndpoint(), c.StatusMessage, want)
		}
		l, err := s.api.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: "projects/proj-1/locations/-"})
		if err != nil {
			t.Fatal(err)
		}
		if len(l.Clusters) != 1 || l.Clusters[0].Endpoint != want {
			t.Errorf("%s: listed = %v, want endpoint %q", tc.name, l.Clusters, want)
		}
	}
}

// Without --gke-bind, a caller from the LAN is told no endpoint, and why,
// never the loopback address, which would be wrong on its machine.
func TestClusterEndpointUnavailableOffLAN(t *testing.T) {
	s := lanTestService(config.IAMOff)
	s.env.Config.Projects = []string{"proj-1"}
	putRunningCluster(t, s, "127.0.0.1:41000")
	c, err := s.api.GetCluster(emu.WithLANCaller(context.Background(), "192.168.1.20"), &containerpb.GetClusterRequest{Name: "projects/proj-1/locations/us-central1/clusters/c"})
	if err != nil {
		t.Fatal(err)
	}
	ipc := c.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig()
	if c.Endpoint != "" || ipc.GetPublicEndpoint() != "" || ipc.GetPrivateEndpoint() != "" {
		t.Errorf("endpoints = %q %q %q, want none", c.Endpoint, ipc.GetPublicEndpoint(), ipc.GetPrivateEndpoint())
	}
	if !strings.Contains(c.StatusMessage, "--gke-bind") || c.Status != containerpb.Cluster_RUNNING {
		t.Errorf("status = %v %q, want RUNNING with a message naming --gke-bind", c.Status, c.StatusMessage)
	}
	w := httptest.NewRecorder()
	s.serveKubeconfig(w, httptest.NewRequestWithContext(emu.WithLANCaller(context.Background(), "192.168.1.20"), "GET", "/_emu/kubeconfig", nil))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "--gke-bind") {
		t.Errorf("kubeconfig for a LAN caller = %d %s, want a refusal naming --gke-bind", w.Code, w.Body)
	}
}
