package secrets_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
)

// Seeding creates secrets with their versions once, whatever the number
// of applications, and the gcloud override points at the gateway.
func TestSeedAndEnvVars(t *testing.T) {
	e := start(t, nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(dir, "seed.yaml")
	data := `secrets:
  secrets:
    - project: ` + testProject + `
      id: api-key
      labels: {team: web}
      versions:
        - data: one
        - dataFile: key.pem
          state: DISABLED
    - project: ` + testProject + `
      id: regional
      location: europe-west1
      versions: [{data: eu}]
`
	if err := os.WriteFile(seed, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.inst.ApplySeed(e.ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	sec := must(e.sm.GetSecret(e.ctx, &secretmanagerpb.GetSecretRequest{Name: parent + "/secrets/api-key"}))
	if sec.GetLabels()["team"] != "web" || sec.GetReplication().GetAutomatic() == nil {
		t.Fatalf("seeded %v", sec)
	}
	acc := must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/secrets/api-key/versions/1"}))
	if string(acc.GetPayload().GetData()) != "one" {
		t.Fatalf("v1 %q", acc.GetPayload().GetData())
	}
	_, err := e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/secrets/api-key/versions/latest"})
	wantCode(t, "disabled seeded version", err, codes.FailedPrecondition)
	_, err = e.sm.GetSecretVersion(e.ctx, &secretmanagerpb.GetSecretVersionRequest{Name: parent + "/secrets/api-key/versions/3"})
	wantCode(t, "seeded twice", err, codes.NotFound)
	acc = must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/locations/europe-west1/secrets/regional/versions/latest"}))
	if string(acc.GetPayload().GetData()) != "eu" {
		t.Fatalf("regional %q", acc.GetPayload().GetData())
	}
	if got := e.inst.EnvVars()["CLOUDSDK_API_ENDPOINT_OVERRIDES_SECRETMANAGER"]; got != e.inst.GatewayURL()+"/secretmanager/" {
		t.Fatalf("gcloud override %q", got)
	}
}

// A Seed file export applied to an emptied Instance recreates the secrets
// with their versions and exports byte for byte the same (FR-CORE-013).
func TestSeedExportRoundTrip(t *testing.T) {
	e := start(t, nil)
	dir := t.TempDir()
	seed := []byte(`secrets:
  secrets:
    - project: ` + testProject + `
      id: api-key
      replicas: [us-east1, europe-west1]
      labels: {team: web, env: dev}
      versions:
        - data: one
        - data: two
          state: DISABLED
        - data: gone
          state: DESTROYED
    - project: ` + testProject + `
      id: regional
      location: europe-west1
      versions: [{data: eu}]
    - project: ` + testProject + `
      id: empty
`)
	if err := e.inst.ApplySeedData(e.ctx, seed, dir); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0xff, 0x00, 0xfe, 'x'}
	must(e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: parent + "/secrets/api-key", Payload: &secretmanagerpb.SecretPayload{Data: binary},
	}))
	first, err := e.inst.ExportSeed(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state: DESTROYED", "state: DISABLED", "!!binary", "location: europe-west1", "- us-east1"} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("export lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(string(first), "gone") {
		t.Fatalf("destroyed payload exported:\n%s", first)
	}
	if err := e.inst.Reset(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.inst.ApplySeedData(e.ctx, first, dir); err != nil {
		t.Fatalf("apply export: %v\n%s", err, first)
	}
	second, err := e.inst.ExportSeed(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", first, second)
	}
	sec := must(e.sm.GetSecret(e.ctx, &secretmanagerpb.GetSecretRequest{Name: parent + "/secrets/api-key"}))
	if sec.GetLabels()["env"] != "dev" || len(sec.GetReplication().GetUserManaged().GetReplicas()) != 2 {
		t.Fatalf("restored %v", sec)
	}
	acc := must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/secrets/api-key/versions/latest"}))
	if string(acc.GetPayload().GetData()) != string(binary) || !strings.HasSuffix(acc.GetName(), "/versions/4") {
		t.Fatalf("latest %s %q", acc.GetName(), acc.GetPayload().GetData())
	}
	v := must(e.sm.GetSecretVersion(e.ctx, &secretmanagerpb.GetSecretVersionRequest{Name: parent + "/secrets/api-key/versions/3"}))
	if v.GetState() != secretmanagerpb.SecretVersion_DESTROYED {
		t.Fatalf("version 3 %v", v.GetState())
	}
}
