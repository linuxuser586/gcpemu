package secrets_test

import (
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Under enforce, payloads need secretmanager.versions.access: a secret
// binding to roles/secretmanager.secretAccessor grants it, and the Editor
// basic role does not.
func TestEnforce(t *testing.T) {
	e := start(t, nil, emutest.WithIAMMode(config.IAMEnforce))
	svc, _ := e.inst.Env.Lookup("iam")
	keys := svc.(emu.ServiceAccountKeys)
	token := func(p string) string {
		tok, _, err := keys.AccessToken(e.ctx, emu.Principal(p))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	owner, reader, editor := token(e.inst.Config.DefaultPrincipal), token("user:reader@example.com"), token("user:editor@example.com")
	call := func(tok, method, path string, body string) int {
		req, _ := http.NewRequestWithContext(e.ctx, method, e.inst.GatewayURL()+"/secretmanager/v1/"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := call(owner, "POST", parent+"/secrets?secretId=s", `{"replication":{"automatic":{}}}`); c != 200 {
		t.Fatalf("owner create: %d", c)
	}
	if c := call(owner, "POST", parent+"/secrets/s:addVersion", `{"payload":{"data":"eA=="}}`); c != 200 {
		t.Fatalf("owner add: %d", c)
	}
	access := parent + "/secrets/s/versions/1:access"
	if c := call(reader, "GET", access, ""); c != http.StatusForbidden {
		t.Fatalf("reader before binding: %d", c)
	}
	if c := call(reader, "GET", parent+"/secrets", ""); c != http.StatusForbidden {
		t.Fatalf("reader list: %d", c)
	}
	_, err := e.sm.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: parent + "/secrets/s", Policy: &iampb.Policy{Bindings: []*iampb.Binding{
		{Role: "roles/secretmanager.secretAccessor", Members: []string{"user:reader@example.com"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if c := call(reader, "GET", access, ""); c != 200 {
		t.Fatalf("reader with secretAccessor: %d", c)
	}
	if c := call(reader, "GET", parent+"/secrets/s", ""); c != http.StatusForbidden {
		t.Fatalf("secretAccessor may not read metadata: %d", c)
	}
	tp, err := e.sm.TestIamPermissions(e.ctx, &iampb.TestIamPermissionsRequest{Resource: parent + "/secrets/s", Permissions: []string{"secretmanager.versions.access"}})
	if err != nil || len(tp.GetPermissions()) != 1 {
		t.Fatalf("testIamPermissions (owner): %v %v", tp, err)
	}

	// The project's Editor reads metadata but not payloads.
	crm := e.inst.GatewayURL() + "/cloudresourcemanager/v1/projects/" + testProject
	req, _ := http.NewRequestWithContext(e.ctx, "POST", crm+":setIamPolicy", strings.NewReader(`{"policy":{"bindings":[
		{"role":"roles/owner","members":["`+e.inst.Config.DefaultPrincipal+`"]},
		{"role":"roles/editor","members":["user:editor@example.com"]}]}}`))
	req.Header.Set("Authorization", "Bearer "+owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("project setIamPolicy: %v %v", resp, err)
	}
	resp.Body.Close()
	if c := call(editor, "GET", parent+"/secrets/s", ""); c != 200 {
		t.Fatalf("editor get: %d", c)
	}
	if c := call(editor, "GET", access, ""); c != http.StatusForbidden {
		t.Fatalf("editor access: %d", c)
	}
}
