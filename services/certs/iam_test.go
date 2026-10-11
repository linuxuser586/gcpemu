package certs_test

import (
	"net/http"
	"testing"
)

// TestTLSPolicyIAM is #51: Network Security serves getIamPolicy,
// setIamPolicy and testIamPermissions on client and server TLS policies;
// Certificate Manager has no IAM methods.
func TestTLSPolicyIAM(t *testing.T) {
	e := start(t)
	e.restOp("POST", nsBase+"/clientTlsPolicies?clientTlsPolicyId=ctp", map[string]any{"sni": "backend.test"})
	res := nsBase + "/clientTlsPolicies/ctp"

	var pol struct {
		Etag     string `json:"etag"`
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	if code := e.rest("GET", res+":getIamPolicy?options.requestedPolicyVersion=3", nil, &pol); code != http.StatusOK || len(pol.Bindings) != 0 {
		t.Fatalf("getIamPolicy: %d %+v", code, pol)
	}
	set := map[string]any{"policy": map[string]any{"etag": pol.Etag, "bindings": []map[string]any{
		{"role": "roles/networksecurity.admin", "members": []string{"user:a@example.com"}},
	}}}
	if code := e.rest("POST", res+":setIamPolicy", set, &pol); code != http.StatusOK || len(pol.Bindings) != 1 {
		t.Fatalf("setIamPolicy: %d %+v", code, pol)
	}
	if code := e.rest("POST", res+":setIamPolicy", set, nil); code != http.StatusConflict {
		t.Errorf("stale etag: %d, want 409", code)
	}
	if code := e.rest("GET", res+":getIamPolicy", nil, &pol); code != http.StatusOK || len(pol.Bindings) != 1 || pol.Bindings[0].Members[0] != "user:a@example.com" {
		t.Errorf("getIamPolicy after set: %d %+v", code, pol)
	}
	var tp struct{ Permissions []string }
	if code := e.rest("POST", res+":testIamPermissions", map[string]any{"permissions": []string{"networksecurity.clientTlsPolicies.get"}}, &tp); code != http.StatusOK || len(tp.Permissions) != 1 {
		t.Errorf("testIamPermissions: %d %+v", code, tp)
	}
	if code := e.rest("GET", nsBase+"/clientTlsPolicies/missing:getIamPolicy", nil, nil); code != http.StatusNotFound {
		t.Errorf("missing policy: %d, want 404", code)
	}
	if code := e.rest("GET", nsBase+"/backendAuthenticationConfigs/bac:getIamPolicy", nil, nil); code != http.StatusNotFound {
		t.Errorf("backend authentication config: %d, want 404", code)
	}
	if code := e.rest("GET", "/certificatemanager/v1/"+globalLoc+"/certificates/c:getIamPolicy", nil, nil); code != http.StatusNotFound {
		t.Errorf("certificate: %d, want 404", code)
	}

	e.restOp("DELETE", res, nil)
	e.restOp("POST", nsBase+"/clientTlsPolicies?clientTlsPolicyId=ctp", map[string]any{"sni": "backend.test"})
	pol.Bindings = nil
	if code := e.rest("GET", res+":getIamPolicy", nil, &pol); code != http.StatusOK || len(pol.Bindings) != 0 {
		t.Errorf("policy after recreate: %d %+v", code, pol)
	}
}
