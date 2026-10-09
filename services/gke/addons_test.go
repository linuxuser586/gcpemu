package gke

import (
	"strings"
	"testing"
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
