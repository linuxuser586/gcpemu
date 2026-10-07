package emutest_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// recordLookups routes every Go name lookup in this process through a DNS
// server that records the names and answers NXDOMAIN, and returns the
// recorded names.
func recordLookups(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var names []string
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &mdns.Server{PacketConn: pc, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, r *mdns.Msg) {
		mu.Lock()
		for _, q := range r.Question {
			names = append(names, strings.TrimSuffix(q.Name, "."))
		}
		mu.Unlock()
		m := new(mdns.Msg)
		m.SetRcode(r, mdns.RcodeNameError)
		_ = w.WriteMsg(m)
	})}
	go srv.ActivateAndServe()
	addr := pc.LocalAddr().String()
	oldGo, oldDial := net.DefaultResolver.PreferGo, net.DefaultResolver.Dial
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", addr)
	}
	t.Cleanup(func() {
		net.DefaultResolver.PreferGo, net.DefaultResolver.Dial = oldGo, oldDial
		_ = srv.Shutdown()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), names...)
	}
}

// realLookingCredentials points ADC (GOOGLE_APPLICATION_CREDENTIALS and
// the gcloud ADC file) at credentials whose token endpoints are Google's.
func realLookingCredentials(t *testing.T) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	dir := t.TempDir()
	sa, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "real-project", "private_key_id": "k1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "real@real-project.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": "https://oauth2.googleapis.com/token",
	})
	saPath := filepath.Join(dir, "sa.json")
	_ = os.WriteFile(saPath, sa, 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", saPath)
	gcloud := filepath.Join(dir, "gcloud")
	_ = os.MkdirAll(gcloud, 0o700)
	user, _ := json.Marshal(map[string]string{"type": "authorized_user", "client_id": "c", "client_secret": "s", "refresh_token": "r"})
	_ = os.WriteFile(filepath.Join(gcloud, "application_default_credentials.json"), user, 0o600)
	t.Setenv("CLOUDSDK_CONFIG", gcloud)
	t.Setenv("HOME", dir)
}

func send(t *testing.T, method, url string, body any, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func ok(t *testing.T, method, url string, body any, hdr map[string]string) []byte {
	t.Helper()
	code, b := send(t, method, url, body, hdr)
	if code/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, url, code, b)
	}
	return b
}

// TestNoGoogleCalls is NFR-SEC-002: with real-looking Google credentials in
// the environment, issuing tokens, signing, the metadata server, STS
// federation, Cloud Storage and Pub/Sub push with OIDC tokens never look up
// (and so never reach) a Google host. Every name lookup in the process is
// recorded; anonymous public image pulls through the registry mirror are
// out of scope (they are the user's image references).
func TestNoGoogleCalls(t *testing.T) {
	realLookingCredentials(t)
	lookups := recordLookups(t)
	if _, err := net.LookupHost("recorder-check.invalid"); err == nil {
		t.Fatal("the recording resolver answered")
	}

	inst := emutest.Start(t, []string{"iam", "gcs", "pubsub", "dns", "compute", "certs"})
	gw := inst.GatewayURL()
	const proj = "nogoogle-proj"
	sa := "pusher@" + proj + ".iam.gserviceaccount.com"

	// IAM: service account, key, tokens, signing, metadata server.
	ok(t, "POST", gw+"/iam/v1/projects/"+proj+"/serviceAccounts", map[string]any{"accountId": "pusher"}, nil)
	ok(t, "POST", gw+"/iam/v1/projects/"+proj+"/serviceAccounts/"+sa+"/keys", map[string]any{}, nil)
	creds := gw + "/iamcredentials/v1/projects/-/serviceAccounts/" + sa
	ok(t, "POST", creds+":generateAccessToken", map[string]any{"scope": []string{"https://www.googleapis.com/auth/cloud-platform"}}, nil)
	ok(t, "POST", creds+":generateIdToken", map[string]any{"audience": "https://app.example.test"}, nil)
	ok(t, "POST", creds+":signBlob", map[string]any{"payload": base64.StdEncoding.EncodeToString([]byte("x"))}, nil)
	md := "http://" + inst.Endpoint("metadata") + "/computeMetadata/v1/instance/service-accounts/default/"
	ok(t, "GET", md+"token", nil, map[string]string{"Metadata-Flavor": "Google"})
	ok(t, "GET", md+"identity?audience=https://app.example.test", nil, map[string]string{"Metadata-Flavor": "Google"})

	// STS: a provider whose issuer is a Google host other than the
	// emulator's own (accounts.google.com, whose keys it holds) is refused
	// rather than fetched.
	num := project.NumberString(proj)
	pools := gw + "/iam/v1/projects/" + proj + "/locations/global/workloadIdentityPools"
	ok(t, "POST", pools+"?workloadIdentityPoolId=pool", map[string]any{}, nil)
	ok(t, "POST", pools+"/pool/providers?workloadIdentityPoolProviderId=google",
		map[string]any{"oidc": map[string]any{"issuerUri": "https://securetoken.google.com/" + proj}, "attributeMapping": map[string]string{"google.subject": "assertion.sub"}}, nil)
	provider := "//iam.googleapis.com/projects/" + num + "/locations/global/workloadIdentityPools/pool/providers/google"
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "https://securetoken.google.com/" + proj, "aud": "https:" + provider,
		"sub": "u", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = "k"
	subject, _ := tok.SignedString(key)
	code, b := send(t, "POST", gw+"/sts/v1/token", map[string]any{
		"grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "audience": provider,
		"subject_token": subject, "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token", "scope": "https://www.googleapis.com/auth/cloud-platform",
	}, nil)
	if code/100 == 2 || !strings.Contains(string(b), "never contacts Google") {
		t.Errorf("STS with a Google issuer: %d %s", code, b)
	}

	// Cloud Storage.
	gcs := "http://" + inst.Endpoint("gcs")
	ok(t, "POST", gcs+"/storage/v1/b?project="+proj, map[string]any{"name": "plain-bucket"}, nil)
	ok(t, "POST", gcs+"/upload/storage/v1/b/plain-bucket/o?uploadType=media&name=o", "data", nil)

	// Pub/Sub push with an OIDC token to a local endpoint.
	got := make(chan string, 1)
	push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("Authorization"):
		default:
		}
	}))
	defer push.Close()
	ok(t, "PUT", gw+"/pubsub/v1/projects/"+proj+"/topics/topic1", map[string]any{}, nil)
	ok(t, "PUT", gw+"/pubsub/v1/projects/"+proj+"/subscriptions/push1", map[string]any{
		"topic":      "projects/" + proj + "/topics/topic1",
		"pushConfig": map[string]any{"pushEndpoint": push.URL + "/push", "oidcToken": map[string]string{"serviceAccountEmail": sa}},
	}, nil)
	ok(t, "POST", gw+"/pubsub/v1/projects/"+proj+"/topics/topic1:publish", map[string]any{"messages": []map[string]string{{"data": "eA=="}}}, nil)
	select {
	case a := <-got:
		if !strings.HasPrefix(a, "Bearer ") {
			t.Errorf("push without an OIDC token: %q", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no push delivery")
	}

	var google []string
	for _, n := range lookups() {
		if emu.IsGoogleHost(n) {
			google = append(google, n)
		}
	}
	if len(google) > 0 {
		t.Errorf("looked up Google hosts: %v", google)
	}
	if ls := lookups(); len(ls) == 0 || ls[0] != "recorder-check.invalid" {
		t.Errorf("recorder saw %v", ls)
	}
}
