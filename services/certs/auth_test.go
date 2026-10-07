package certs_test

import (
	"net/http"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// In enforce mode the default principal (owner) holds the
// certificatemanager.* permissions; a principal without roles is denied.
func TestEnforce(t *testing.T) {
	e := start(t, emutest.WithIAMMode(config.IAMEnforce))
	svc, ok := e.inst.Env.Lookup("iam")
	if !ok {
		t.Skip("iam not running")
	}
	keys := svc.(emu.ServiceAccountKeys)
	owner, _, err := keys.AccessToken(e.ctx, emu.Principal(e.inst.Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	nobody, _, err := keys.AccessToken(e.ctx, emu.Principal("user:nobody@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	call := func(tok, method, path string) int {
		req, _ := http.NewRequestWithContext(e.ctx, method, e.inst.GatewayURL()+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := call(owner, "POST", cmBase+"/certificateMaps?certificateMapId=m"); c != 200 {
		t.Fatalf("owner create: %d", c)
	}
	if c := call(owner, "GET", nsBase+"/serverTlsPolicies"); c != 200 {
		t.Fatalf("owner list: %d", c)
	}
	for _, p := range []string{cmBase + "/certificateMaps/m", cmBase + "/certificateMaps", nsBase + "/backendAuthenticationConfigs"} {
		if c := call(nobody, "GET", p); c != http.StatusForbidden {
			t.Fatalf("nobody GET %s: %d", p, c)
		}
	}
}
