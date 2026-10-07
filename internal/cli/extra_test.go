package cli

import "testing"

// The google provider (>= 7) creates service accounts through the
// "IAM beta" endpoint setting; without it tofu would call iam.googleapis.com.
func TestProviderEndpointsIAMBeta(t *testing.T) {
	for _, e := range providerEndpoints {
		if e.setting == "iam_beta_custom_endpoint" && e.service == "iam" && e.path == "/iam/v1/" {
			return
		}
	}
	t.Fatal("tofu-provider does not set iam_beta_custom_endpoint")
}
