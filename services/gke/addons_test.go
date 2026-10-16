package gke

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The embedded add-on manifests decode, and every object has a known
// path; the Secret Manager add-on registers GKE's driver name.
func TestAddonManifests(t *testing.T) {
	for _, name := range []string{"gateway-api-" + gatewayAPIVer + "-standard.yaml.gz", "gateway-api-" + gatewayAPIVer + "-experimental.yaml.gz", "secret-manager.yaml", "secret-sync.yaml"} {
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
