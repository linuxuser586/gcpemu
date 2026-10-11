package iam_test

import (
	"net/http"
	"net/url"
	"testing"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/project"
)

const idpMetadata = `<?xml version="1.0"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.com">
  <md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.com/sso"/>
  </md:IDPSSODescriptor>
</md:EntityDescriptor>`

// TestWorkloadIdentityProviderKinds is #51: AWS and SAML providers can be
// managed (google_iam_workload_identity_pool_provider with aws or saml),
// but exchanging their credentials is refused.
func TestWorkloadIdentityProviderKinds(t *testing.T) {
	inst := startIAM(t)
	c := iamClient(t, inst)
	pools := c.Projects.Locations.WorkloadIdentityPools
	if _, err := pools.Create("projects/"+testProject+"/locations/global", &iamv1.WorkloadIdentityPool{}).
		WorkloadIdentityPoolId("ext-pool").Do(); err != nil {
		t.Fatal(err)
	}
	pool := "projects/" + project.NumberString(testProject) + "/locations/global/workloadIdentityPools/ext-pool"
	create := func(id string, p *iamv1.WorkloadIdentityPoolProvider) error {
		_, err := pools.Providers.Create(pool, p).WorkloadIdentityPoolProviderId(id).Do()
		return err
	}

	if err := create("aws-prov", &iamv1.WorkloadIdentityPoolProvider{Aws: &iamv1.Aws{AccountId: "999999999999"}}); err != nil {
		t.Fatalf("aws: %v", err)
	}
	got, err := pools.Providers.Get(pool + "/providers/aws-prov").Do()
	if err != nil || got.Aws == nil || got.Aws.AccountId != "999999999999" || len(got.AttributeMapping) != 0 || got.State != "ACTIVE" {
		t.Errorf("aws provider = %+v, %v", got, err)
	}
	saml := &iamv1.WorkloadIdentityPoolProvider{
		Saml:             &iamv1.Saml{IdpMetadataXml: idpMetadata},
		AttributeMapping: map[string]string{"google.subject": "assertion.subject"},
	}
	if err := create("saml-prov", saml); err != nil {
		t.Fatalf("saml: %v", err)
	}
	if got, err := pools.Providers.Get(pool + "/providers/saml-prov").Do(); err != nil || got.Saml == nil || got.Saml.IdpMetadataXml != idpMetadata {
		t.Errorf("saml provider = %+v, %v", got, err)
	}

	for name, p := range map[string]*iamv1.WorkloadIdentityPoolProvider{
		"bad-account":  {Aws: &iamv1.Aws{AccountId: "1234"}},
		"two-kinds":    {Aws: &iamv1.Aws{AccountId: "999999999999"}, Oidc: &iamv1.Oidc{IssuerUri: "https://issuer.example"}},
		"none":         {AttributeMapping: map[string]string{"google.subject": "assertion.sub"}},
		"bad-metadata": {Saml: &iamv1.Saml{IdpMetadataXml: "<html/>"}, AttributeMapping: saml.AttributeMapping},
		"saml-no-sub":  {Saml: &iamv1.Saml{IdpMetadataXml: idpMetadata}},
	} {
		if err := create("p-"+name, p); httpCode(err) != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}

	resp, err := http.PostForm(inst.GatewayURL()+"/sts/v1/token", url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token_type": {"urn:ietf:params:aws:token-type:aws4_request"},
		"subject_token":      {"{}"},
		"audience":           {"//iam.googleapis.com/" + pool + "/providers/aws-prov"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("AWS exchange: status %d, want 400", resp.StatusCode)
	}
}
