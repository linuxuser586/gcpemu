package iam_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	iamv1 "google.golang.org/api/iam/v1"
)

// refreshToken mints an access token for principal through the refresh
// token grant, as the Web console's policy simulator does; it returns the
// grant's HTTP status and token.
func refreshToken(t *testing.T, inst *testInstance, principal string) (int, string) {
	t.Helper()
	resp, err := http.PostForm(inst.GatewayURL()+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {principal},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.AccessToken
}

// call sends method path with body (JSON) and an optional bearer token,
// and returns the status and decoded response.
func call(t *testing.T, method, u, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, u, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestSwitchEnforcement is the admin API's /_emu/v1/iam/enforcement
// (FR-IAM-004, SRS 4.8.3): the level the Instance started with is
// switched while it runs, later calls follow it, and /_emu/v1/info
// reports it.
func TestSwitchEnforcement(t *testing.T) {
	inst := startIAM(t)
	base := inst.GatewayURL()
	level := func() string {
		code, out := call(t, http.MethodGet, base+"/_emu/v1/iam/enforcement", "", nil)
		if code != 200 {
			t.Fatalf("GET enforcement: %d %v", code, out)
		}
		_, info := call(t, http.MethodGet, base+"/_emu/v1/info", "", nil)
		if info["iamMode"] != out["level"] {
			t.Errorf("info.iamMode = %v, enforcement = %v", info["iamMode"], out["level"])
		}
		return out["level"].(string)
	}
	set := func(l string) (int, map[string]any) {
		return call(t, http.MethodPut, base+"/_emu/v1/iam/enforcement", "", map[string]string{"level": l})
	}
	if got := level(); got != "audit" {
		t.Fatalf("level = %q, want audit", got)
	}

	// A principal with no roles: audit lets it read the project's policy,
	// enforce does not, and switching back lets it again.
	code, tok := refreshToken(t, inst, "user:nobody@example.com")
	if code != 200 || tok == "" {
		t.Fatalf("refresh grant: %d", code)
	}
	policy := base + "/cloudresourcemanager/v1/projects/" + testProject + ":getIamPolicy"
	if code, out := call(t, http.MethodPost, policy, tok, map[string]any{}); code != 200 {
		t.Fatalf("audit: %d %v", code, out)
	}
	if code, out := set("enforce"); code != 200 || out["level"] != "enforce" {
		t.Fatalf("PUT enforce: %d %v", code, out)
	}
	if got := level(); got != "enforce" {
		t.Fatalf("level = %q, want enforce", got)
	}
	if code, out := call(t, http.MethodPost, policy, tok, map[string]any{}); code != 403 {
		t.Fatalf("enforce: want 403, got %d %v", code, out)
	}
	// The Default principal (no credentials) is owner of every project.
	if code, out := call(t, http.MethodPost, policy, "", map[string]any{}); code != 200 {
		t.Fatalf("enforce, Default principal: %d %v", code, out)
	}
	if code, _ := set("audit"); code != 200 {
		t.Fatalf("PUT audit: %d", code)
	}
	if code, out := call(t, http.MethodPost, policy, tok, map[string]any{}); code != 200 {
		t.Fatalf("back to audit: %d %v", code, out)
	}

	code, out := set("strict")
	if code != 400 || !strings.Contains(out["error"].(string), `invalid IAM enforcement level "strict"`) {
		t.Fatalf("PUT strict: %d %v", code, out)
	}
	if got := level(); got != "audit" {
		t.Fatalf("level after a bad PUT = %q, want audit", got)
	}
	if code, _ := set("off"); code != 200 || level() != "off" {
		t.Fatalf("PUT off: %d", code)
	}
}

// TestRefreshGrantDisabledAccount: a disabled service account gets no
// access token from the refresh token grant, as from the JWT bearer grant.
func TestRefreshGrantDisabledAccount(t *testing.T) {
	inst := startIAM(t)
	admin := iamClient(t, inst)
	sa := createSA(t, admin, "sim-target")
	if code, _ := refreshToken(t, inst, "serviceAccount:"+sa.Email); code != 200 {
		t.Fatalf("enabled account: %d", code)
	}
	if _, err := admin.Projects.ServiceAccounts.Disable("projects/-/serviceAccounts/"+sa.Email, &iamv1.DisableServiceAccountRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if code, _ := refreshToken(t, inst, "serviceAccount:"+sa.Email); code != 400 {
		t.Fatalf("disabled account: want 400, got %d", code)
	}
}
