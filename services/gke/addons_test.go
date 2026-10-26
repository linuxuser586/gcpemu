package gke

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// The embedded add-on manifests decode, and every object has a known
// path; the Secret Manager add-on registers GKE's driver name.
func TestAddonManifests(t *testing.T) {
	for _, name := range []string{"gateway-api-" + gatewayAPIVer + "-standard.yaml.gz", "gateway-api-" + gatewayAPIVer + "-experimental.yaml.gz", "secret-manager.yaml", "secret-sync.yaml", "managed-otel.yaml"} {
		objs, err := manifest(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(objs) == 0 {
			t.Fatalf("%s: no objects", name)
		}
		for _, o := range objs {
			if _, _, err := objectPath(o); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	std, _ := manifest("gateway-api-" + gatewayAPIVer + "-standard.yaml.gz")
	exp, _ := manifest("gateway-api-" + gatewayAPIVer + "-experimental.yaml.gz")
	if extra := without(exp, std); len(extra) == 0 || len(extra) >= len(exp) {
		t.Errorf("experimental-only objects: %d of %d", len(extra), len(exp))
	}
	b, _ := addonFS.ReadFile("addons/secret-manager.yaml")
	for _, want := range []string{"name: secrets-store-gke.csi.k8s.io", "--drivername=secrets-store-gke.csi.k8s.io", "/var/run/secrets-store-csi-providers"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("secret-manager.yaml lacks %q", want)
		}
	}
}

func TestParseSPCSecrets(t *testing.T) {
	got, err := parseSPCSecrets(`
- resourceName: "projects/p/secrets/a/versions/latest"
  path: "a.txt"
- resourceName: "projects/p/locations/us-central1/secrets/b/versions/1"
  fileName: "b.txt"
`)
	if err != nil || len(got) != 2 || got[0].Path != "a.txt" || got[1].Path != "b.txt" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := parseSPCSecrets(`- {resourceName: x, path: ../etc/passwd}`); err == nil {
		t.Error("path escaping the volume accepted")
	}
}

// applyAddon returns only once the CRDs it applied are Established; a CRD
// that never is fails the wait, naming it, when the context ends (#310).
func TestApplyAddonWaitsForEstablished(t *testing.T) {
	crd := func(name string) map[string]any {
		return map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": name}}
	}
	path := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"
	established := func(k *fakeKube, name string) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.objs[path+name]["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "NamesAccepted", "status": "True"},
			map[string]any{"type": "Established", "status": "True"},
		}}
	}

	t.Run("Established", func(t *testing.T) {
		k, kc := newFakeKube(t)
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if k.get(path+"a.example.com") != nil && k.get(path+"b.example.com") != nil {
					time.Sleep(700 * time.Millisecond)
					established(k, "a.example.com")
					established(k, "b.example.com")
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		start := time.Now()
		if err := applyAddon(context.Background(), kc, "test", []map[string]any{crd("a.example.com"), crd("b.example.com")}); err != nil {
			t.Fatalf("applyAddon: %v", err)
		}
		if d := time.Since(start); d < 700*time.Millisecond {
			t.Errorf("applyAddon returned after %v, before the CRDs were Established", d)
		}
	})

	t.Run("AlreadyEstablished", func(t *testing.T) {
		k, kc := newFakeKube(t)
		k.put(`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"a.example.com"},
			"status":{"conditions":[{"type":"Established","status":"True"}]}}`)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if err := awaitEstablished(ctx, kc, []string{"a.example.com"}, time.Minute); err != nil {
			t.Fatalf("awaitEstablished: %v", err)
		}
	})

	t.Run("NeverEstablished", func(t *testing.T) {
		_, kc := newFakeKube(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := applyAddon(ctx, kc, "test", []map[string]any{crd("stuck.example.com")})
		if err == nil || !strings.Contains(err.Error(), "stuck.example.com") || !strings.Contains(err.Error(), "not established") {
			t.Fatalf("applyAddon = %v; want an error naming stuck.example.com", err)
		}
	})

	t.Run("Timeout", func(t *testing.T) {
		k, kc := newFakeKube(t)
		k.put(`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"stuck.example.com"}}`)
		start := time.Now()
		err := awaitEstablished(context.Background(), kc, []string{"stuck.example.com"}, 300*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "stuck.example.com") {
			t.Fatalf("awaitEstablished = %v; want an error naming stuck.example.com", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("awaitEstablished took %v past its timeout", d)
		}
	})
}

// The Gateway API add-on owns its CRDs and installs no safe-upgrades
// policy, so that one update moves a cluster between channels; a policy
// an older emulator installed goes, one the user installed stays (#358).
func TestGatewayAPISafeUpgrades(t *testing.T) {
	const (
		policy  = "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/" + safeUpgrades
		binding = "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/" + safeUpgrades
		tcp     = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/tcproutes.gateway.networking.k8s.io"
	)
	ctx := context.Background()
	std, exp := containerpb.GatewayAPIConfig_CHANNEL_STANDARD, containerpb.GatewayAPIConfig_CHANNEL_EXPERIMENTAL
	// fixture enforces the policy's first rule while the policy exists and
	// for the next stale requests after it is deleted, as the API server's
	// policy cache lags.
	fixture := func(t *testing.T, stale int) (*Service, *fakeKube, *kubeClient) {
		k, kc := newFakeKube(t)
		k.establish = true
		k.admit = func(method, path string, old, obj map[string]any) (int, string) {
			channel := func(o map[string]any) any {
				return field(o, "metadata.annotations").(map[string]any)["gateway.networking.k8s.io/channel"]
			}
			if obj["kind"] != "CustomResourceDefinition" || old == nil || channel(old) != "standard" || channel(obj) != "experimental" {
				return 0, ""
			}
			if k.objs[policy] == nil {
				if stale == 0 {
					return 0, ""
				}
				stale--
			}
			return http.StatusUnprocessableEntity, `{"kind":"Status","message":"customresourcedefinitions.apiextensions.k8s.io \"` + path +
				`\" is forbidden: ValidatingAdmissionPolicy 'safe-upgrades.gateway.networking.k8s.io' with binding 'safe-upgrades.gateway.networking.k8s.io' denied request: Installing experimental CRDs on top of standard channel CRDs is prohibited by default.","reason":"Invalid","code":422}`
		}
		return snapshotService(t), k, kc
	}
	// legacy installs the standard manifest as it was before #358, the
	// policy and binding included.
	legacy := func(t *testing.T, kc *kubeClient) {
		objs, err := manifest(GatewayAPIManifest("standard"))
		if err != nil {
			t.Fatal(err)
		}
		if err := applyAddon(ctx, kc, gatewayAPIAddon, objs); err != nil {
			t.Fatalf("legacy install: %v", err)
		}
	}

	t.Run("NotInstalled", func(t *testing.T) {
		s, k, kc := fixture(t, 0)
		for _, ch := range []containerpb.GatewayAPIConfig_Channel{std, exp, std, exp} {
			if err := s.reconcileGatewayAPI(ctx, kc, ch); err != nil {
				t.Fatalf("reconcile %v: %v", ch, err)
			}
			if k.get(policy) != nil || k.get(binding) != nil {
				t.Fatalf("%v: the safe-upgrades policy or binding is installed", ch)
			}
		}
		if k.get(tcp) == nil {
			t.Error("experimental CRD missing on the experimental channel")
		}
	})

	t.Run("LabelledRemoved", func(t *testing.T) {
		s, k, kc := fixture(t, 3)
		legacy(t, kc)
		if k.get(policy) == nil || k.get(binding) == nil {
			t.Fatal("legacy install lacks the policy or binding")
		}
		if err := s.reconcileGatewayAPI(ctx, kc, exp); err != nil {
			t.Fatalf("standard → experimental: %v", err)
		}
		if k.get(policy) != nil || k.get(binding) != nil {
			t.Error("the add-on's safe-upgrades policy or binding survived the reconcile")
		}
		if k.get(tcp) == nil {
			t.Error("experimental CRD missing after the update")
		}
	})

	t.Run("UserPolicyKept", func(t *testing.T) {
		s, k, kc := fixture(t, 0)
		for _, o := range []string{
			`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingAdmissionPolicy","metadata":{"name":"` + safeUpgrades + `"}}`,
			`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingAdmissionPolicyBinding","metadata":{"name":"` + safeUpgrades + `"}}`,
		} {
			k.put(o)
		}
		if err := s.reconcileGatewayAPI(ctx, kc, std); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if k.get(policy) == nil || k.get(binding) == nil {
			t.Fatal("the user's safe-upgrades policy or binding was deleted")
		}
		if l := field(k.get(policy), "metadata.labels"); l != nil {
			t.Errorf("the user's policy was relabelled: %v", l)
		}
		// The user's policy keeps denying: the update fails, bounded.
		defer func(d time.Duration) { safeUpgradesGrace = d }(safeUpgradesGrace)
		safeUpgradesGrace = time.Second
		start := time.Now()
		err := s.reconcileGatewayAPI(ctx, kc, exp)
		if err == nil || !strings.Contains(err.Error(), safeUpgrades) {
			t.Fatalf("standard → experimental under the user's policy = %v; want its denial", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("the denied reconcile took %v", d)
		}
	})
}
