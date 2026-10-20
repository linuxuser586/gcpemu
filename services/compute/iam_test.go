package compute_test

import (
	"context"
	"net/http"
	"testing"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// Compute checks the exact compute.* permissions (FR-INT-012).
func TestIAMEnforce(t *testing.T) {
	inst, admin, _ := newClients(t, emutest.WithIAMMode(config.IAMEnforce))
	ctx := context.Background()
	customNet(t, admin, "vpc")
	svc, _ := inst.Internal().Env.Lookup("iam")
	keys := svc.(emu.ServiceAccountKeys)
	tok, _, err := keys.AccessToken(ctx, emu.Principal("user:viewer@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithHTTPClient(&http.Client{Transport: bearer{tok}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Networks.Get(proj, "vpc").Do(); code(err) != http.StatusForbidden {
		t.Fatalf("get without role: %v", err)
	}
	pol := []byte(`{"bindings":[{"role":"roles/compute.networkViewer","members":["user:viewer@example.com"]}]}`)
	if _, err := svc.(emu.IAMPolicyStore).SetPolicyJSON(ctx, "//cloudresourcemanager.googleapis.com/projects/"+proj, pol); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Networks.Get(proj, "vpc").Do(); err != nil {
		t.Fatalf("get as network viewer: %v", err)
	}
	if _, err := c.Networks.List(proj).Do(); err != nil {
		t.Fatalf("list as network viewer: %v", err)
	}
	if _, err := c.Networks.Delete(proj, "vpc").Do(); code(err) != http.StatusForbidden {
		t.Fatalf("delete as viewer: %v", err)
	}
}

// TestSubnetworkIAM is #51: google_compute_subnetwork_iam_* read and write
// the subnetwork's policy, and deleting the subnetwork drops it.
func TestSubnetworkIAM(t *testing.T) {
	_, c, _ := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "s", "us-central1", "10.10.0.0/24")
	p, err := c.Subnetworks.GetIamPolicy(proj, "us-central1", "s").OptionsRequestedPolicyVersion(3).Do()
	if err != nil || len(p.Bindings) != 0 {
		t.Fatalf("initial policy = %+v, %v", p, err)
	}
	p.Bindings = []*computev1.Binding{{Role: "roles/compute.networkUser", Members: []string{"user:a@example.com"}}}
	set, err := c.Subnetworks.SetIamPolicy(proj, "us-central1", "s", &computev1.RegionSetPolicyRequest{Policy: p}).Do()
	if err != nil || len(set.Bindings) != 1 || set.Etag == "" || set.Etag == p.Etag {
		t.Fatalf("setIamPolicy = %+v, %v", set, err)
	}
	if _, err := c.Subnetworks.SetIamPolicy(proj, "us-central1", "s", &computev1.RegionSetPolicyRequest{Policy: p}).Do(); code(err) != http.StatusConflict {
		t.Errorf("stale etag: want 409, got %v", err)
	}
	got, err := c.Subnetworks.GetIamPolicy(proj, "us-central1", "s").Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:a@example.com" || got.Etag != set.Etag {
		t.Errorf("getIamPolicy = %+v, %v", got, err)
	}
	tp, err := c.Subnetworks.TestIamPermissions(proj, "us-central1", "s", &computev1.TestPermissionsRequest{Permissions: []string{"compute.subnetworks.use"}}).Do()
	if err != nil || len(tp.Permissions) != 1 {
		t.Errorf("testIamPermissions = %+v, %v", tp, err)
	}
	if _, err := c.Subnetworks.GetIamPolicy(proj, "us-central1", "missing").Do(); code(err) != http.StatusNotFound {
		t.Errorf("missing subnetwork: want 404, got %v", err)
	}
	waitDo(t, c)(c.Subnetworks.Delete(proj, "us-central1", "s").Do())
	subnet(t, c, "vpc", "s", "us-central1", "10.10.0.0/24")
	if p, err := c.Subnetworks.GetIamPolicy(proj, "us-central1", "s").Do(); err != nil || len(p.Bindings) != 0 {
		t.Errorf("policy after recreate = %+v, %v", p, err)
	}
}
