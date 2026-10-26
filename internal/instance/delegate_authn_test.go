package instance

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// withLAN runs Artifact Registry, served on the LAN at 127.0.0.1, and the
// Google front end there too.
func withLAN(c *config.Config) {
	withAR(c)
	c.GFEBind = []string{"127.0.0.1"}
	c.Ports[frontend.LANEndpoint] = 0
}

// officeToken has the office issue an access token for the service
// account runner of the Delegated Project.
func officeToken(t *testing.T, office *Instance) string {
	t.Helper()
	svc, _ := office.Env.Lookup("iam")
	tok, _, err := svc.(emu.ServiceAccountKeys).AccessToken(context.Background(), emu.Principal("serviceAccount:"+runner))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// lanGet gets path from in's --gfe-bind listener for host with the
// access token tok, returning the status and body.
func lanGet(t *testing.T, in *Instance, host, path, tok string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: in.Env.CA.Pool(), ServerName: host},
	}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+in.Env.Endpoints.Get(frontend.LANEndpoint)+path, nil)
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// dockerLogin checks the access token tok at in's --ar-bind registry, as
// `docker login -u oauth2accesstoken` does, returning the status and body.
func dockerLogin(t *testing.T, in *Instance, tok string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: in.Env.CA.Pool()},
	}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+in.Env.Endpoints.Get("ar-lan")+"/v2/", nil)
	req.SetBasicAuth("oauth2accesstoken", tok)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A token the office issued for a service account of the Delegated
// Project works on every path of the laptop that checks tokens: its
// authenticator, the API gateway, the Google front end's LAN listener and
// a docker login at its registry. With the office down, a token only it
// might have issued fails each path as unavailable, naming the office, in
// every IAM mode, never as the default Principal or anonymously; a token
// that is no Instance's, or the laptop's own for a disabled account,
// keeps its outcome (#736).
func TestDelegatedTokenOnEveryPath(t *testing.T) {
	laptop, office := delegated(t, withLAN, nil)
	callAdmin(t, office, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	up, down := officeToken(t, office), officeToken(t, office)
	const local = "builder@laptop-own.iam.gserviceaccount.com"
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/laptop-own/serviceAccounts", map[string]string{"accountId": "builder"}, nil)
	svc, _ := laptop.Env.Lookup("iam")
	disabled, _, err := svc.(emu.ServiceAccountKeys).AccessToken(context.Background(), emu.Principal("serviceAccount:"+local))
	if err != nil {
		t.Fatal(err)
	}
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/laptop-own/serviceAccounts/"+local+":disable", map[string]any{}, nil)
	const topics = "/pubsub/v1/projects/laptop-own/topics"

	if p, err := laptop.Env.Auth.Authenticate(t.Context(), up); err != nil || p != emu.Principal("serviceAccount:"+runner) {
		t.Errorf("the laptop's authenticator knows the office's token as %q %v, want %s", p, err, runner)
	}
	if code, body := bearerGet(t, laptop, topics, up); code != http.StatusOK {
		t.Errorf("the laptop's topics with the office's token = %d %s", code, body)
	}
	if code, body := lanGet(t, laptop, "pubsub.googleapis.com", "/v1/projects/laptop-own/topics", up); code != http.StatusOK {
		t.Errorf("the laptop's topics on its LAN listener with the office's token = %d %s", code, body)
	}
	if code, body := dockerLogin(t, laptop, up); code != http.StatusOK {
		t.Errorf("docker login at the laptop's registry with the office's token = %d %s", code, body)
	}
	if code, body := dockerLogin(t, laptop, "ya29.gcpemu.nobodys"); code != http.StatusUnauthorized {
		t.Errorf("docker login with a token no Instance issued = %d %s, want 401", code, body)
	}

	if err := office.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	pa := laptop.Env.Auth.(*emu.PolicyAuthorizer)
	for _, mode := range []string{config.IAMEnforce, config.IAMAudit, config.IAMOff} {
		pa.SetMode(mode)
		named := func(what string, code int, body string) {
			t.Helper()
			if code != http.StatusServiceUnavailable || !strings.Contains(body, `office`) {
				t.Errorf("%s with the office down, in %s mode = %d %s, want 503 naming the office", what, mode, code, body)
			}
		}
		_, err := laptop.Env.Auth.Authenticate(t.Context(), down)
		if apierr.From(err).Code != codes.Unavailable || !strings.Contains(err.Error(), `"office"`) {
			t.Errorf("the laptop's authenticator with the office down, in %s mode: %v, want UNAVAILABLE naming the office", mode, err)
		}
		code, body := bearerGet(t, laptop, topics, down)
		named("the gateway", code, body)
		code, body = lanGet(t, laptop, "pubsub.googleapis.com", "/v1/projects/laptop-own/topics", down)
		named("the LAN listener", code, body)
		code, body = dockerLogin(t, laptop, down)
		named("docker login", code, body)
		if !strings.Contains(body, "UNAVAILABLE") {
			t.Errorf("docker login with the office down, in %s mode = %s, want a registry UNAVAILABLE error", mode, body)
		}
	}
	// A token the laptop issued itself, for an account since disabled, and
	// one no Instance could have issued are not sent over a link.
	if _, err := laptop.Env.Auth.Authenticate(t.Context(), disabled); !errors.Is(err, emu.ErrUnknownToken) {
		t.Errorf("the laptop's own token for a disabled account, with the office down: %v, want unknown", err)
	}
	if _, err := laptop.Env.Auth.Authenticate(t.Context(), "not-an-emulator-token"); !errors.Is(err, emu.ErrUnknownToken) {
		t.Errorf("a token without the emulator's prefix, with the office down: %v, want unknown", err)
	}
}

// Instances that each delegate a Project to the other answer a token
// neither issued as unknown, each asking the other once, without asking
// back in circles (#736).
func TestMutualDelegationAsksNoCircles(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": "laptop-own"}, nil)
	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": "laptop-own", "link": "laptop"}, nil)
	for _, in := range []*Instance{laptop, office} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := in.Env.Auth.Authenticate(ctx, "ya29.gcpemu.nobodys-"+in.Config.Instance)
		cancel()
		if !errors.Is(err, emu.ErrUnknownToken) {
			t.Errorf("%s's authenticator with a token neither issued: %v, want unknown", in.Config.Instance, err)
		}
	}
}
