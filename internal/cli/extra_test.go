package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestTofuProviderBeta is #112: --beta adds a google-beta block with the
// same endpoints except container's v1beta1, and the default output stays a
// single google block.
func TestTofuProviderBeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"services":["gke","gcs"]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		var out bytes.Buffer
		root := New(nil)
		root.SetOut(&out)
		root.SetArgs(append([]string{"tofu-provider", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("tofu-provider %v: %v", args, err)
		}
		return out.String()
	}
	plain := run()
	if strings.Contains(plain, "google-beta") || strings.Contains(plain, "v1beta1") {
		t.Errorf("default output has a beta block:\n%s", plain)
	}
	out := run("--beta")
	google, beta, ok := strings.Cut(out, `provider "google-beta" {`)
	if !ok {
		t.Fatalf("no google-beta block:\n%s", out)
	}
	if !strings.HasSuffix(plain, strings.TrimSpace(google)+"\n") {
		t.Errorf("--beta changed the google block:\n%s", out)
	}
	for _, want := range []string{
		`container_custom_endpoint            = "` + srv.URL + `/container/v1beta1/"`,
		`storage_custom_endpoint              = "` + srv.URL + `/storage/v1/"`,
		`access_token = "owner"`,
	} {
		if !strings.Contains(beta, want) {
			t.Errorf("google-beta block lacks %s:\n%s", want, beta)
		}
	}
	if strings.Contains(beta, "/container/v1/") {
		t.Errorf("google-beta block uses container v1:\n%s", beta)
	}
}
