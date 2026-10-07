package iam_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2/google"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/services/iam"
)

// TestMetadataServer covers FR-IAM-008 with the official metadata client
// and the compute token source used by ADC on GCE.
func TestMetadataServer(t *testing.T) {
	inst := startIAM(t)
	md := inst.Endpoint("metadata")
	t.Setenv("GCE_METADATA_HOST", md)
	ctx := context.Background()
	c := metadata.NewClient(nil)

	if !metadata.OnGCE() {
		t.Fatal("OnGCE false with GCE_METADATA_HOST set")
	}
	pid, err := c.ProjectIDWithContext(ctx)
	if err != nil || pid != iam.DefaultProjectID {
		t.Fatalf("project id: %q %v", pid, err)
	}
	num, err := c.NumericProjectIDWithContext(ctx)
	if err != nil || num != project.NumberString(iam.DefaultProjectID) {
		t.Fatalf("numeric id: %q %v", num, err)
	}
	zone, err := c.ZoneWithContext(ctx)
	if err != nil || zone != "us-central1-a" {
		t.Fatalf("zone: %q %v", zone, err)
	}
	email, err := c.EmailWithContext(ctx, "default")
	if err != nil || email != num+"-compute@developer.gserviceaccount.com" {
		t.Fatalf("email: %q %v", email, err)
	}
	if ud, err := c.GetWithContext(ctx, "universe/universe-domain"); err != nil || ud != "googleapis.com" {
		t.Fatalf("universe domain: %q %v", ud, err)
	}
	idt, err := c.GetWithContext(ctx, "instance/service-accounts/default/identity?audience=https://x.example&format=full")
	if err != nil {
		t.Fatal(err)
	}
	if claims := verifyIDToken(t, inst, idt); claims["email"] != email {
		t.Fatalf("identity claims: %v", claims)
	}

	// The compute token source (ADC on GCE) gets a token for the default SA.
	tok, err := google.ComputeTokenSource("").Token()
	if err != nil {
		t.Fatal(err)
	}
	info := fetchJSON[map[string]string](t, inst.GatewayURL()+"/oauth2/tokeninfo?access_token="+tok.AccessToken)
	if info["email"] != email {
		t.Fatalf("tokeninfo: %v", info)
	}

	// Header rules.
	resp, err := http.Get("http://" + md + "/computeMetadata/v1/project/project-id")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Metadata-Flavor") != "Google" {
		t.Fatalf("missing header: %s %v", resp.Status, resp.Header)
	}
	resp, err = http.Get("http://" + md + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Metadata-Flavor") != "Google" {
		t.Fatalf("root: %s", resp.Status)
	}

	// recursive=true renders JSON.
	req, _ := http.NewRequest("GET", "http://"+md+"/computeMetadata/v1/project/?recursive=true", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil || rec["projectId"] != iam.DefaultProjectID {
		t.Fatalf("recursive: %s %v", b, err)
	}
}

// TestMetadataIdentityRights checks the host metadata default SA acts with
// the default principal's rights until a service account is configured.
func TestMetadataIdentityRights(t *testing.T) {
	inst := startIAM(t, enforce)
	t.Setenv("GCE_METADATA_HOST", inst.Endpoint("metadata"))
	getPolicy := func() error {
		ts := google.ComputeTokenSource("")
		_, err := crmClient(t, inst, option.WithTokenSource(ts)).Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do()
		return err
	}
	if err := getPolicy(); err != nil {
		t.Fatalf("implicit metadata SA: %v", err)
	}
	createSA(t, iamClient(t, inst), "vm-runner")
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	_ = os.WriteFile(seed, []byte("iam:\n  metadata: {serviceAccount: vm-runner@test-project.iam.gserviceaccount.com}\n"), 0o600)
	if err := inst.ApplySeed(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	if err := getPolicy(); httpCode(err) != 403 {
		t.Fatalf("explicit metadata SA without roles: want 403, got %v", err)
	}
}
