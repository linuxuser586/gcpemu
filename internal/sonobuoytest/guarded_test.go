package sonobuoytest

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/escapeguard"
)

// fakeSonobuoy, set in the environment, makes the test binary a sonobuoy
// that GETs the URL it is given, as Go's default client does: through
// the proxy the environment names.
const fakeSonobuoy = "GCPEMU_FAKE_SONOBUOY"

func TestMain(m *testing.M) {
	if os.Getenv(fakeSonobuoy) != "" {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(os.Args[len(os.Args)-1])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		resp.Body.Close()
		fmt.Println(resp.Status)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// guarded returns a Guarded whose sonobuoy is the fake.
func guarded(t *testing.T, apiServer string) Guarded {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Guarded{Bin: exe, Env: append(os.Environ(), fakeSonobuoy+"=1"), Guard: escapeguard.New(t), APIServer: apiServer}
}

func TestGuardedReportsEscapes(t *testing.T) {
	g := guarded(t, "")
	out, escaped, _ := g.exec(time.Minute, "get", "https://escape.gcpemu.invalid/")
	if !slices.Equal(escaped, []string{"escape.gcpemu.invalid"}) {
		t.Errorf("escaped = %v, want [escape.gcpemu.invalid]; sonobuoy said:\n%s", escaped, out)
	}
}

// The API server is reached directly; here it does not resolve, but the
// guard never sees it.
func TestGuardedExemptsAPIServer(t *testing.T) {
	for _, apiServer := range []string{"https://kube.gcpemu.invalid:6443", "kube.gcpemu.invalid"} {
		g := guarded(t, apiServer)
		out, escaped, _ := g.exec(time.Minute, "get", "https://kube.gcpemu.invalid:6443/version")
		if len(escaped) != 0 {
			t.Errorf("APIServer %s: escaped = %v, want none; sonobuoy said:\n%s", apiServer, escaped, out)
		}
	}
}
