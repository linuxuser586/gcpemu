package build_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// secret creates the Secret Manager secret id in the test project with
// one version holding payload.
func (e *env) secret(id, payload string) {
	e.t.Helper()
	base := "/secretmanager/v1/projects/" + testProject + "/secrets"
	e.send(http.MethodPost, base+"?secretId="+id, `{"replication":{"automatic":{}}}`)
	e.send(http.MethodPost, base+"/"+id+":addVersion", `{"payload":{"data":"`+base64.StdEncoding.EncodeToString([]byte(payload))+`"}}`)
}

// secretBuild is a Build whose step reads the latest version of the
// Secret Manager secret id as TOKEN, through a versionName with a
// substitution.
func secretBuild(id string) *cloudbuildpb.Build {
	return &cloudbuildpb.Build{
		AvailableSecrets: &cloudbuildpb.Secrets{SecretManager: []*cloudbuildpb.SecretManagerSecret{
			{VersionName: "projects/$PROJECT_ID/secrets/" + id + "/versions/latest", Env: "TOKEN"},
		}},
		Steps: []*cloudbuildpb.BuildStep{{Name: busybox, Entrypoint: "sh", SecretEnv: []string{"TOKEN"},
			Args: []string{"-c", `echo "plain $$TOKEN"; echo "$$TOKEN" | rev`}}},
	}
}

// A step that lists an availableSecrets env in secretEnv sees the Secret
// version's payload in it; the payload is masked in the log and is
// nowhere in the Build. A disabled version fails the Build.
func TestSecretEnv(t *testing.T) {
	e := startWith(t, []string{"secrets"})
	e.secret("token", "s3cr3t-value")
	e.secret("key", "-----BEGIN KEY-----\nline-one-of-the-key\nline-two-of-the-key\n-----END KEY-----\n")
	pem := secretBuild("token")
	pem.AvailableSecrets.SecretManager = append(pem.AvailableSecrets.SecretManager,
		&cloudbuildpb.SecretManagerSecret{VersionName: "projects/" + testProject + "/secrets/key/versions/1", Env: "KEY"})
	pem.Steps[0].SecretEnv = append(pem.Steps[0].SecretEnv, "KEY")
	pem.Steps[0].Args[1] += `; echo "$$KEY"`
	b := e.run(pem)
	log := e.log(b)
	if !strings.Contains(log, "eulav-t3rc3s") {
		t.Errorf("the step did not see the payload:\n%s", log)
	}
	if strings.Contains(log, "s3cr3t-value") || strings.Contains(log, "line-one-of-the-key") || !strings.Contains(log, "plain [REDACTED]") {
		t.Errorf("the payload is not masked in the log:\n%s", log)
	}
	if stored := e.storedLog(b); strings.Contains(stored, "s3cr3t-value") {
		t.Errorf("the payload is in the log object:\n%s", stored)
	}
	if j, _ := protojson.Marshal(e.get(b.GetId())); strings.Contains(string(j), "s3cr3t-value") {
		t.Errorf("the payload is in the Build: %s", j)
	}
	if got := b.GetAvailableSecrets().GetSecretManager()[0].GetVersionName(); got != "projects/"+testProject+"/secrets/token/versions/latest" {
		t.Errorf("versionName = %q, want it substituted", got)
	}

	e.send(http.MethodPost, "/secretmanager/v1/projects/"+testProject+"/secrets/token/versions/1:disable", `{}`)
	op, created := e.create(secretBuild("token"))
	if _, err := op.Wait(e.ctx); err == nil {
		t.Fatal("a Build with a disabled Secret version succeeded")
	}
	b = e.get(created.GetId())
	if b.GetStatus() != cloudbuildpb.Build_FAILURE || !strings.Contains(b.GetStatusDetail(), "projects/"+testProject+"/secrets/token/versions/latest") {
		t.Errorf("Build = %v (%s), want FAILURE naming the version", b.GetStatus(), b.GetStatusDetail())
	}
}

// Under enforce, the Build's service account needs
// secretmanager.versions.access on the Secret.
func TestSecretEnvUnderEnforce(t *testing.T) {
	e := startWith(t, []string{"secrets", "iam"}, emutest.WithIAMMode(config.IAMEnforce))
	e.ctx = e.as(e.inst.Internal().Config.DefaultPrincipal)
	e.secret("token", "s3cr3t-value")
	b := secretBuild("token")
	b.ServiceAccount = "projects/" + testProject + "/serviceAccounts/" + e.serviceAccount("nosecrets")
	op, created := e.create(b)
	if _, err := op.Wait(e.ctx); err == nil {
		t.Fatal("the Build read the Secret without secretmanager.versions.access")
	}
	if b := e.get(created.GetId()); b.GetStatus() != cloudbuildpb.Build_FAILURE || !strings.Contains(b.GetStatusDetail(), "secretmanager.versions.access") {
		t.Errorf("Build = %v (%s)", b.GetStatus(), b.GetStatusDetail())
	}
}

// builds.create refuses a secretEnv no availableSecrets defines and one
// defined but unused, and answers UNIMPLEMENTED for Cloud KMS secrets and
// availableSecrets.inline, naming the field.
func TestSecretsValidation(t *testing.T) {
	e := start(t)
	undefined := secretBuild("token")
	undefined.Steps[0].SecretEnv = []string{"TOKEN", "OTHER"}
	wantInvalid(t, e.createErr(undefined), `invalid build: secretEnv "OTHER" is used without being defined`)
	unused := secretBuild("token")
	unused.Steps[0].SecretEnv = nil
	wantInvalid(t, e.createErr(unused), `invalid build: secretEnv "TOKEN" is defined without being used`)

	kms := secretBuild("token")
	kms.Secrets = []*cloudbuildpb.Secret{{KmsKeyName: "projects/p/locations/global/keyRings/r/cryptoKeys/k", SecretEnv: map[string][]byte{"K": []byte("x")}}}
	inline := secretBuild("token")
	inline.AvailableSecrets.Inline = []*cloudbuildpb.InlineSecret{{KmsKeyName: "projects/p/locations/global/keyRings/r/cryptoKeys/k"}}
	for field, b := range map[string]*cloudbuildpb.Build{"secrets": kms, "availableSecrets.inline": inline} {
		if err := e.createErr(b); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want UNIMPLEMENTED naming it", field, err)
		}
	}
}
