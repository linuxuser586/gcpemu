package secrets_test

import (
	"os"
	"path/filepath"
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
