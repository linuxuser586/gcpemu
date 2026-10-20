package secrets_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// denyDestroy attaches a deny policy to the test project refusing
// secretmanager.googleapis.com/versions.destroy to everyone but
// breakglass@example.com, as polyappic guards its seal keys (#184).
func denyDestroy(e *env) string {
	e.t.Helper()
	parent := "/iam/v2/policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/"+testProject) + "/denypolicies"
	var op struct {
		Done     bool
		Response struct{ Name string }
	}
	code := e.rest("POST", parent+"?policyId=no-destroy", map[string]any{
		"rules": []any{map[string]any{"denyRule": map[string]any{
			"deniedPrincipals":    []string{"principalSet://goog/public:all"},
			"exceptionPrincipals": []string{"principal://goog/subject/breakglass@example.com"},
			"deniedPermissions":   []string{"secretmanager.googleapis.com/versions.destroy"},
		}}},
	}, &op)
	if code != 200 || !op.Done {
		e.t.Fatalf("create deny policy: %d %+v", code, op)
	}
	return op.Response.Name
}

// TestDenyPolicy: under enforce, the project's owner cannot destroy a
// Secret version a deny policy guards, while its exception principal can;
// under audit the destroy succeeds and the denials log names the policy.
func TestDenyPolicy(t *testing.T) {
	e := start(t, []string{"iam"}, emutest.WithIAMMode(config.IAMEnforce))
	svc, _ := e.inst.Internal().Env.Lookup("iam")
	keys := svc.(emu.ServiceAccountKeys)
	token := func(p string) string {
		tok, _, err := keys.AccessToken(e.ctx, emu.Principal(p))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	owner, breakglass := token(e.inst.Internal().Config.DefaultPrincipal), token("user:breakglass@example.com")
	call := func(tok, method, path, body string) (int, string) {
		req, _ := http.NewRequestWithContext(e.ctx, method, e.inst.GatewayURL()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	secret := "/secretmanager/v1/" + parent + "/secrets/seal"
	if c, out := call(owner, "POST", "/secretmanager/v1/"+parent+"/secrets?secretId=seal", `{"replication":{"automatic":{}}}`); c != 200 {
		t.Fatalf("create secret: %d %s", c, out)
	}
	for range 3 {
		if c, out := call(owner, "POST", secret+":addVersion", `{"payload":{"data":"eA=="}}`); c != 200 {
			t.Fatalf("add version: %d %s", c, out)
		}
	}
	policy := denyDestroy(e)
	// breakglass holds the destroy permission through an allow policy.
	if c, out := call(owner, "POST", secret+":setIamPolicy", `{"policy":{"bindings":[{"role":"roles/secretmanager.admin","members":["user:breakglass@example.com"]}]}}`); c != 200 {
		t.Fatalf("setIamPolicy: %d %s", c, out)
	}

	c, out := call(owner, "POST", secret+"/versions/1:destroy", `{}`)
	if c != http.StatusForbidden || !strings.Contains(out, "PERMISSION_DENIED") {
		t.Fatalf("owner destroy under enforce: %d %s", c, out)
	}
	if c, out := call(breakglass, "POST", secret+"/versions/1:destroy", `{}`); c != 200 || !strings.Contains(out, `"DESTROYED"`) {
		t.Fatalf("exception principal destroy: %d %s", c, out)
	}
	// The deny policy guards destroy only.
	if c, out := call(owner, "POST", secret+"/versions/2:disable", `{}`); c != 200 {
		t.Fatalf("owner disable: %d %s", c, out)
	}

	var level struct{ Level string }
	if c := e.rest("PUT", "/_emu/v1/iam/enforcement", map[string]string{"level": "audit"}, &level); c != 200 {
		t.Fatalf("PUT audit: %d", c)
	}
	if c, out := call(owner, "POST", secret+"/versions/3:destroy", `{}`); c != 200 {
		t.Fatalf("owner destroy under audit: %d %s", c, out)
	}
	var denials struct {
		Denials []emu.Denial
	}
	if c := e.rest("GET", "/_emu/v1/iam/denials", nil, &denials); c != 200 {
		t.Fatalf("GET denials: %d", c)
	}
	levels := map[string]string{}
	for _, d := range denials.Denials {
		if d.DenyPolicy == "" {
			continue
		}
		v := d.Resource[strings.LastIndexByte(d.Resource, '/')+1:]
		levels[v] = d.Level
		if d.DenyPolicy != policy || d.Principal != e.inst.Internal().Config.DefaultPrincipal ||
			d.Permission != "secretmanager.versions.destroy" || d.Count != 1 ||
			d.Role != "" || d.Granted || d.Project != testProject {
			t.Errorf("denial = %+v", d)
		}
	}
	if len(levels) != 2 || levels["1"] != "enforce" || levels["3"] != "audit" {
		t.Errorf("deny-policy denials by version = %v, want 1 enforce and 3 audit", levels)
	}
}
