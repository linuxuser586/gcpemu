package iam_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// providerWithMapping creates an OIDC provider whose attributeMapping holds
// google.subject and attribute.repo, and returns the providers client and
// the provider's name and the IAM REST base URL.
func providerWithMapping(t *testing.T) (provs *iamv1.ProjectsLocationsWorkloadIdentityPoolsProvidersService, name, restBase string) {
	t.Helper()
	inst := startIAM(t)
	pools := iamClient(t, inst).Projects.Locations.WorkloadIdentityPools
	if _, err := pools.Create("projects/"+testProject+"/locations/global", &iamv1.WorkloadIdentityPool{}).
		WorkloadIdentityPoolId("map-pool").Do(); err != nil {
		t.Fatal(err)
	}
	pool := "projects/" + project.NumberString(testProject) + "/locations/global/workloadIdentityPools/map-pool"
	if _, err := pools.Providers.Create(pool, &iamv1.WorkloadIdentityPoolProvider{
		Oidc: &iamv1.Oidc{IssuerUri: "https://issuer.example"},
		AttributeMapping: map[string]string{
			"google.subject": "assertion.sub",
			"attribute.repo": "assertion.repository",
		},
	}).WorkloadIdentityPoolProviderId("map-prov").Do(); err != nil {
		t.Fatal(err)
	}
	return pools.Providers, pool + "/providers/map-prov", inst.GatewayURL() + "/iam/v1/"
}

// mappingOf returns the stored attributeMapping of the provider.
func mappingOf(t *testing.T, provs *iamv1.ProjectsLocationsWorkloadIdentityPoolsProvidersService, name string) map[string]string {
	t.Helper()
	p, err := provs.Get(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	return p.AttributeMapping
}

// TestProviderMaskKeyChangesThatKeyOnly is #1088 (ADR 0075): a mask path
// naming one attributeMapping key changes that key and keeps the others.
func TestProviderMaskKeyChangesThatKeyOnly(t *testing.T) {
	for _, path := range []string{"attributeMapping.`attribute.repo`", "attribute_mapping.`attribute.repo`"} {
		t.Run(path, func(t *testing.T) {
			provs, name, _ := providerWithMapping(t)
			patch := &iamv1.WorkloadIdentityPoolProvider{AttributeMapping: map[string]string{"attribute.repo": "assertion.repo"}}
			if _, err := provs.Patch(name, patch).UpdateMask(path).Do(); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"google.subject": "assertion.sub", "attribute.repo": "assertion.repo"}
			if got := mappingOf(t, provs, name); !maps.Equal(got, want) {
				t.Errorf("attributeMapping = %v, want %v", got, want)
			}
		})
	}
}

// TestProviderMaskKeyMissingFromRequestIsRemoved is #1088: a key path the
// request's map lacks removes that key, and when that leaves the mapping
// without google.subject the update is rejected and nothing changes.
func TestProviderMaskKeyMissingFromRequestIsRemoved(t *testing.T) {
	provs, name, _ := providerWithMapping(t)
	noMapping := &iamv1.WorkloadIdentityPoolProvider{DisplayName: "renamed"}
	op, err := provs.Patch(name, noMapping).UpdateMask("attributeMapping.`attribute.repo`").Do()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"google.subject": "assertion.sub"}
	if got := mappingOf(t, provs, name); !maps.Equal(got, want) {
		t.Errorf("after removing attribute.repo: attributeMapping = %v, want %v", got, want)
	}
	var resp iamv1.WorkloadIdentityPoolProvider
	if err := json.Unmarshal(op.Response, &resp); err != nil || !maps.Equal(resp.AttributeMapping, want) {
		t.Errorf("Patch response attributeMapping = %v (%v), want %v", resp.AttributeMapping, err, want)
	}

	_, err = provs.Patch(name, noMapping).UpdateMask("displayName,attributeMapping.`google.subject`").Do()
	if httpCode(err) != http.StatusBadRequest {
		t.Fatalf("removing google.subject: want 400, got %v", err)
	}
	p, err := provs.Get(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(p.AttributeMapping, want) || p.DisplayName != "" {
		t.Errorf("rejected update changed the provider: %+v", p)
	}
}

// TestProviderMaskKeyNonStringValueRejected is #1088: a mapping value the
// request names by key must be a string; the update is rejected, not stored
// unvalidated.
func TestProviderMaskKeyNonStringValueRejected(t *testing.T) {
	provs, name, restBase := providerWithMapping(t)
	u := restBase + name + "?updateMask=" + url.QueryEscape("attributeMapping.`attribute.repo`")
	req, err := http.NewRequest(http.MethodPatch, u, strings.NewReader(`{"attributeMapping":{"attribute.repo":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	want := map[string]string{"google.subject": "assertion.sub", "attribute.repo": "assertion.repository"}
	if got := mappingOf(t, provs, name); !maps.Equal(got, want) {
		t.Errorf("attributeMapping = %v, want %v", got, want)
	}
}

// TestProviderMaskKeyPathRejected is #1088: a key path that is not one
// segment, or one on a field that is not a map, is INVALID_ARGUMENT and
// changes nothing.
func TestProviderMaskKeyPathRejected(t *testing.T) {
	provs, name, _ := providerWithMapping(t)
	patch := &iamv1.WorkloadIdentityPoolProvider{
		DisplayName:      "renamed",
		AttributeMapping: map[string]string{"google.subject": "assertion.email"},
	}
	for _, path := range []string{"attributeMapping.google.subject", "displayName.x", "attributeMapping.`google.subject"} {
		t.Run(path, func(t *testing.T) {
			if _, err := provs.Patch(name, patch).UpdateMask(path).Do(); httpCode(err) != http.StatusBadRequest {
				t.Errorf("want 400, got %v", err)
			}
		})
	}
	p, err := provs.Get(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"google.subject": "assertion.sub", "attribute.repo": "assertion.repository"}
	if !maps.Equal(p.AttributeMapping, want) || p.DisplayName != "" {
		t.Errorf("rejected updates changed the provider: %+v", p)
	}
}

// TestProviderMaskWholeMapWinsOverKey is #1088: a mask naming both
// attributeMapping and one of its keys replaces the whole map.
func TestProviderMaskWholeMapWinsOverKey(t *testing.T) {
	provs, name, _ := providerWithMapping(t)
	want := map[string]string{"google.subject": "assertion.email"}
	patch := &iamv1.WorkloadIdentityPoolProvider{AttributeMapping: want}
	if _, err := provs.Patch(name, patch).UpdateMask("attributeMapping.`attribute.repo`,attributeMapping").Do(); err != nil {
		t.Fatal(err)
	}
	if got := mappingOf(t, provs, name); !maps.Equal(got, want) {
		t.Errorf("attributeMapping = %v, want %v", got, want)
	}
}

// TestProviderMaskNestedMessagePath keeps a path into a nested message,
// as Terraform sends (oidc.issuer_uri), replacing that whole field.
func TestProviderMaskNestedMessagePath(t *testing.T) {
	provs, name, _ := providerWithMapping(t)
	patch := &iamv1.WorkloadIdentityPoolProvider{Oidc: &iamv1.Oidc{IssuerUri: "https://other.example"}}
	if _, err := provs.Patch(name, patch).UpdateMask("oidc.issuer_uri").Do(); err != nil {
		t.Fatal(err)
	}
	if p, err := provs.Get(name).Do(); err != nil || p.Oidc == nil || p.Oidc.IssuerUri != "https://other.example" {
		t.Errorf("provider = %+v, %v", p, err)
	}
}
