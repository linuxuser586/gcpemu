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
	svc, _ := inst.Env.Lookup("iam")
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
