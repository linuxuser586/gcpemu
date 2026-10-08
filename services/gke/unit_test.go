package gke

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		in   string
		ch   containerpb.ReleaseChannel_Channel
		want string
		err  bool
	}{
		{"", containerpb.ReleaseChannel_REGULAR, defaultVersion, false},
		{"-", containerpb.ReleaseChannel_UNSPECIFIED, defaultVersion, false},
		{"", containerpb.ReleaseChannel_RAPID, "1.37.1-gke.100", false},
		{"", containerpb.ReleaseChannel_STABLE, "1.35.9-gke.100", false},
		{"latest", 0, "1.37.1-gke.100", false},
		{"1.36", 0, "1.36.5-gke.100", false},
		{"1.36.4", 0, "1.36.4-gke.100", false},
		{"1.35.8-gke.100", 0, "1.35.8-gke.100", false},
		{"1.30", 0, "", true},
		{"1.3", 0, "", true},
	}
	for _, tt := range tests {
		got, err := resolveVersion(tt.in, tt.ch)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("resolveVersion(%q, %v) = %q, %v; want %q (err %v)", tt.in, tt.ch, got, err, tt.want, tt.err)
		}
	}
	if compareVersions("1.36.5-gke.100", "1.36.10-gke.1") >= 0 {
		t.Error("compareVersions must compare numerically")
	}
	if err := checkSkew("1.36.5-gke.100", "1.37.1-gke.100"); err == nil {
		t.Error("nodes newer than the master must be rejected")
	}
	if minorDiff("1.35.9-gke.100", "1.37.1-gke.100") != 2 {
		t.Error("minorDiff")
	}
	sc := serverConfig()
	if sc.DefaultClusterVersion != defaultVersion || len(sc.Channels) != 4 || len(sc.ValidMasterVersions) != len(versions) {
		t.Errorf("serverConfig = %+v", sc)
	}
	for _, v := range versions {
		if !strings.HasPrefix(v.Image, "rancher/k3s:v"+strings.SplitN(v.GKE, "-", 2)[0]+"-k3s") {
			t.Errorf("version %s maps to image %s", v.GKE, v.Image)
		}
	}
}

func TestLocations(t *testing.T) {
	for _, z := range []string{"us-central1-a", "us-central1-f", "europe-west1-b"} {
		if !isZone(z) || validateLocation(z) != nil {
			t.Errorf("%s should be a zone", z)
		}
	}
	for _, bad := range []string{"us-central1-z", "europe-west1-a", "mars-north1", "us-central1-"} {
		if validateLocation(bad) == nil {
			t.Errorf("%s should be invalid", bad)
		}
	}
	if !isRegion("us-east1") || isZone("us-east1") {
		t.Error("us-east1 is a region")
	}
	if got := defaultZones("us-central1"); strings.Join(got, ",") != "us-central1-a,us-central1-b,us-central1-c" {
		t.Errorf("defaultZones = %v", got)
	}
	if regionOf("asia-east1-b") != "asia-east1" {
		t.Error("regionOf")
	}
}

func TestParseRef(t *testing.T) {
	r, err := parseRef("projects/p/locations/us-central1/clusters/c/nodePools/np", "", "", "", "", 2)
	if err != nil || r.Project != "p" || r.Location != "us-central1" || r.Cluster != "c" || r.Pool != "np" {
		t.Fatalf("parseRef = %+v, %v", r, err)
	}
	r, err = parseRef("", "p", "us-central1-a", "c", "", 1)
	if err != nil || r.clusterName() != "projects/p/locations/us-central1-a/clusters/c" {
		t.Fatalf("legacy parseRef = %+v, %v", r, err)
	}
	if _, err := parseRef("projects/p/locations/l", "", "", "", "", 1); err == nil {
		t.Error("missing cluster accepted")
	}
	if _, err := parseRef("", "p", "", "c", "", 1); err == nil {
		t.Error("missing zone accepted")
	}
	if got := selfLink("p", "us-central1-a", "clusters/c"); got != "https://container.googleapis.com/v1/projects/p/zones/us-central1-a/clusters/c" {
		t.Errorf("zonal selfLink = %s", got)
	}
	if got := selfLink("p", "us-central1", "clusters/c"); got != "https://container.googleapis.com/v1/projects/p/locations/us-central1/clusters/c" {
		t.Errorf("regional selfLink = %s", got)
	}
	for name, ok := range map[string]bool{"c1": true, "my-cluster": true, "1c": false, "C": false, "a-": false, strings.Repeat("a", 41): false} {
		if (validateName("cluster", name) == nil) != ok {
			t.Errorf("validateName(%q) ok=%v", name, !ok)
		}
	}
}

func TestK8sPermission(t *testing.T) {
	tests := []struct {
		ra   resourceAttrs
		want string
	}{
		{resourceAttrs{Verb: "get", Resource: "pods"}, "container.pods.get"},
		{resourceAttrs{Verb: "watch", Resource: "pods"}, "container.pods.list"},
		{resourceAttrs{Verb: "patch", Group: "apps", Resource: "deployments"}, "container.deployments.update"},
		{resourceAttrs{Verb: "create", Resource: "pods", Subresource: "exec"}, "container.pods.exec"},
		{resourceAttrs{Verb: "get", Resource: "pods", Subresource: "log"}, "container.pods.getLogs"},
		{resourceAttrs{Verb: "update", Group: "apps", Resource: "statefulsets", Subresource: "status"}, "container.statefulSets.updateStatus"},
		{resourceAttrs{Verb: "list", Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"}, "container.clusterRoleBindings.list"},
		{resourceAttrs{Verb: "get", Group: "networking.istio.io", Resource: "virtualservices"}, "container.thirdPartyObjects.get"},
		{resourceAttrs{Verb: "frobnicate", Resource: "pods"}, ""},
	}
	for _, tt := range tests {
		if got := k8sPermission(&tt.ra); got != tt.want {
			t.Errorf("k8sPermission(%+v) = %q, want %q", tt.ra, got, tt.want)
		}
	}
	if principalFor("sa@p.iam.gserviceaccount.com", "user:dev@example.com") != "serviceAccount:sa@p.iam.gserviceaccount.com" {
		t.Error("principalFor SA")
	}
	if principalFor("DEV@example.com", "user:dev@example.com") != "user:dev@example.com" {
		t.Error("principalFor default")
	}
}

func TestNodeConfigHelpers(t *testing.T) {
	hosts := arRegistryHosts()
	if len(hosts) < 40 || hosts[0] != "africa-south1-docker.pkg.dev" {
		t.Errorf("arRegistryHosts = %v", hosts[:3])
	}
	c := &containerpb.Cluster{WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{WorkloadPool: "p.svc.id.goog"}}
	np := &containerpb.NodePool{Name: "pool", Config: &containerpb.NodeConfig{
		MachineType: "e2-standard-4", DiskType: "pd-ssd", ImageType: "COS_CONTAINERD", Labels: map[string]string{"team": "web"},
		WorkloadMetadataConfig: &containerpb.WorkloadMetadataConfig{Mode: containerpb.WorkloadMetadataConfig_GKE_METADATA},
	}}
	ls := strings.Join(nodeLabels(c, np, nodeRecord{Zone: "us-central1-a"}), " ")
	for _, want := range []string{"cloud.google.com/gke-nodepool=pool", "team=web", "topology.kubernetes.io/zone=us-central1-a",
		"topology.kubernetes.io/region=us-central1", "iam.gke.io/gke-metadata-server-enabled=true", "cloud.google.com/machine-family=e2"} {
		if !strings.Contains(ls, want) {
			t.Errorf("node labels %q lack %q", ls, want)
		}
	}
	if taintEffect(containerpb.NodeTaint_NO_EXECUTE) != "NoExecute" {
		t.Error("taintEffect")
	}
	n := negName("0123456789abcdef", "default", "web", 80)
	if !strings.HasPrefix(n, "k8s1-01234567-default-web-80-") || len(n) > 63 {
		t.Errorf("negName = %q", n)
	}
	if a, l := saLeaf("/computeMetadata/v1/instance/service-accounts/default/token"); a != "default" || l != "token" {
		t.Errorf("saLeaf = %q %q", a, l)
	}
	k := webhookKubeconfig("http://10.0.0.1:4510/x")
	if !strings.Contains(string(k), "server: http://10.0.0.1:4510/x") {
		t.Error("webhook kubeconfig")
	}
}

func TestPrivateFlags(t *testing.T) {
	f := false
	c := &containerpb.Cluster{PrivateClusterConfig: &containerpb.PrivateClusterConfig{EnablePrivateNodes: true}}
	np := &containerpb.NodePool{}
	if !privateNodes(c, np) {
		t.Error("cluster private nodes")
	}
	np.NetworkConfig = &containerpb.NodeNetworkConfig{EnablePrivateNodes: &f}
	if privateNodes(c, np) {
		t.Error("pool override")
	}
	c.ControlPlaneEndpointsConfig = &containerpb.ControlPlaneEndpointsConfig{IpEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{EnablePublicEndpoint: &f}}
	if !privateEndpoint(c) {
		t.Error("private endpoint")
	}
}

// TestNonLinuxHost is FR-GKE-012 / NFR-PORT-003: on a non-Linux host the
// service refuses to start with a clear message and reports not-ready.
func TestNonLinuxHost(t *testing.T) {
	old := hostOS
	hostOS = "darwin"
	defer func() { hostOS = old }()
	cfg := config.Defaults()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()})
	err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "only supported on Linux") || !strings.Contains(err.Error(), "without the gke service on darwin") {
		t.Fatalf("Start = %v", err)
	}
	if err := s.Ready(); err == nil || !strings.Contains(err.Error(), "Linux") {
		t.Fatalf("Ready = %v", err)
	}
}

// TestPreloadLists is FR-GKE-006/007: every supported version preloads its
// system images, so private nodes (no egress) start offline.
func TestPreloadLists(t *testing.T) {
	for _, v := range versions {
		want := map[string]bool{"rancher/mirrored-pause:": false, "rancher/mirrored-coredns-coredns:": false, "rancher/local-path-provisioner:": false, "rancher/klipper-lb:": false}
		for _, img := range v.Preload {
			for p := range want {
				if strings.HasPrefix(img, p) {
					want[p] = true
				}
			}
		}
		for p, ok := range want {
			if !ok {
				t.Errorf("%s preloads no %s image: %v", v.GKE, p, v.Preload)
			}
		}
	}
}

// fakeIAM is an iam peer without emu.IAMPermissionTester; fakeTesterIAM
// adds it with a fixed verdict.
type fakeIAM struct{}

func (fakeIAM) Name() string                { return "iam" }
func (fakeIAM) Register(emu.Router) error   { return nil }
func (fakeIAM) Start(context.Context) error { return nil }
func (fakeIAM) Stop(context.Context) error  { return nil }
func (fakeIAM) Ready() error                { return nil }

type fakeTesterIAM struct {
	fakeIAM
	grant bool
}

func (f fakeTesterIAM) TestPermissions(_ context.Context, _ string, perms []string) []string {
	if f.grant {
		return perms
	}
	return nil
}

// TestWIAllowedFailsClosed is FR-GKE-005: outside IAM mode off, Workload
// Identity impersonation is denied when the iam service is absent or cannot
// test permissions, and otherwise follows the policy verdict.
func TestWIAllowedFailsClosed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const member, gsa = "serviceAccount:p.svc.id.goog[ns/ksa]", "app@p.iam.gserviceaccount.com"
	tests := []struct {
		name string
		mode string
		iam  emu.Service
		want bool
	}{
		{"off, no iam", config.IAMOff, nil, true},
		{"enforce, no iam", config.IAMEnforce, nil, false},
		{"audit, no iam", config.IAMAudit, nil, false},
		{"enforce, no tester", config.IAMEnforce, fakeIAM{}, false},
		{"enforce, denied", config.IAMEnforce, fakeTesterIAM{}, false},
		{"enforce, granted", config.IAMEnforce, fakeTesterIAM{grant: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
				Auth: emu.NewPolicyAuthorizer(tt.mode, log), Endpoints: emu.NewEndpoints()}
			if tt.iam != nil {
				env.SetServices(map[string]emu.Service{"iam": tt.iam})
			}
			if got := New(env).(*Service).wiAllowed(context.Background(), member, gsa); got != tt.want {
				t.Fatalf("wiAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}
