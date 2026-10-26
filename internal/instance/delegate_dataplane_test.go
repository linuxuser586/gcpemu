package instance

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// withAR runs Artifact Registry too, served on the LAN at 127.0.0.1.
func withAR(c *config.Config) {
	c.Services = append(c.Services, "ar")
	c.ARBind = []string{"127.0.0.1"}
}

// A Linked Instance answers a request that came over a link with the
// Data plane addresses of its LAN listeners, which the caller's machine
// can reach (ADR 0027). A repository read through the laptop names the office's --ar-bind
// address, from which an image pushes and pulls by the name the API
// reported. Read on the office itself, it keeps the pkg.dev name.
func TestDelegatedRepositoryNamesARBind(t *testing.T) {
	laptop, office := delegated(t, nil, withAR)
	const repos = "/artifactregistry/v1/projects/" + shared + "/locations/us-central1/repositories"
	callAdmin(t, office, http.MethodPost, repos+"?repositoryId=images", map[string]string{"format": "DOCKER"}, nil)
	// Creating a repository is an operation.
	for i := 0; ; i++ {
		code, body := adminStatus(t, office, http.MethodGet, repos+"/images", nil)
		if code == http.StatusOK {
			break
		}
		if i == 100 {
			t.Fatalf("repository: %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var repo struct{ RegistryUri string }
	callAdmin(t, laptop, http.MethodGet, repos+"/images", nil, &repo)
	lan := office.Env.Endpoints.Get("ar-lan")
	if want := lan + "/us-central1-docker.pkg.dev/" + shared + "/images"; repo.RegistryUri != want {
		t.Fatalf("registryUri through the laptop = %q, want %q", repo.RegistryUri, want)
	}
	var local struct{ RegistryUri string }
	callAdmin(t, office, http.MethodGet, repos+"/images", nil, &local)
	if want := "us-central1-docker.pkg.dev/" + shared + "/images"; local.RegistryUri != want {
		t.Errorf("registryUri on the office = %q, want %q", local.RegistryUri, want)
	}

	svc, _ := office.Env.Lookup("iam")
	tok, _, err := svc.(emu.ServiceAccountKeys).AccessToken(context.Background(), emu.Principal(office.Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(repo.RegistryUri + "/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: office.Env.CA.Pool()}}
	auth := remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok})
	img, _ := random.Image(256, 1)
	if err := remote.Write(ref, img, auth, remote.WithTransport(tr)); err != nil {
		t.Fatalf("push to %s: %v", ref, err)
	}
	if _, err := remote.Image(ref, auth, remote.WithTransport(tr)); err != nil {
		t.Fatalf("pull from %s: %v", ref, err)
	}

	var imgs struct{ DockerImages []struct{ Uri string } }
	callAdmin(t, laptop, http.MethodGet, repos+"/images/dockerImages", nil, &imgs)
	d, _ := img.Digest()
	if want := repo.RegistryUri + "/app@" + d.String(); len(imgs.DockerImages) != 1 || imgs.DockerImages[0].Uri != want {
		t.Errorf("docker images through the laptop = %+v, want one at %s/app", imgs.DockerImages, repo.RegistryUri)
	}
}

// A key created over a link through a real Google hostname, as a caller
// on the LAN or in a pod reaches the front Instance through the Google
// front end, names Google's endpoints and universe (ADR 0021), which that
// caller reaches by the same names.
func TestDelegatedKeyThroughGoogleHostname(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	req, _ := http.NewRequest(http.MethodPost, "http://"+laptop.Env.Endpoints.Get("gateway")+"/v1/projects/-/serviceAccounts/"+runner+"/keys", strings.NewReader("{}"))
	req.Host = "iam.googleapis.com"
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var key struct{ PrivateKeyData string }
	if err := json.NewDecoder(resp.Body).Decode(&key); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("creating a key through iam.googleapis.com = %d %v", resp.StatusCode, err)
	}
	b, _ := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	var file struct {
		keyFileURLs
		KeyID          string `json:"private_key_id"`
		UniverseDomain string `json:"universe_domain"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	want := keyFileURLs{
		TokenURI:                "https://oauth2.googleapis.com/token",
		AuthProviderX509CertURL: "https://www.googleapis.com/oauth2/v1/certs",
		ClientX509CertURL:       "https://www.googleapis.com/robot/v1/metadata/x509/" + runner,
	}
	if file.keyFileURLs != want || file.UniverseDomain != "googleapis.com" {
		t.Errorf("key file names %+v in universe %q, want Google's", file.keyFileURLs, file.UniverseDomain)
	}
	var keys struct{ Keys []struct{ Name string } }
	callAdmin(t, office, http.MethodGet, "/iam/v1/projects/"+shared+"/serviceAccounts/"+runner+"/keys?keyTypes=USER_MANAGED", nil, &keys)
	if len(keys.Keys) != 1 || !strings.HasSuffix(keys.Keys[0].Name, "/"+file.KeyID) {
		t.Errorf("the office's keys = %+v, want the one created over the link", keys.Keys)
	}

	// Its client_x509_cert_url, through the front Instance, serves its
	// certificate from the office (#692).
	u, _ := url.Parse(file.ClientX509CertURL)
	req, _ = http.NewRequest(http.MethodGet, "http://"+laptop.Env.Endpoints.Get("gateway")+u.EscapedPath(), nil)
	req.Host = u.Host
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var certs map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&certs); err != nil || resp.StatusCode != http.StatusOK || certs[file.KeyID] == "" {
		t.Errorf("GET %s through the front Instance = %d %v %v, want key %s", file.ClientX509CertURL, resp.StatusCode, certs, err, file.KeyID)
	}
}
