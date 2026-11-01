//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/sonobuoytest"
)

// pinned is a downloadable tool: url and sha256 by GOOS/GOARCH, and the
// file to take from a .tar.gz (empty for a plain binary). helm and
// cloud-sql-proxy have no Windows pins: they only run against Cloud SQL
// and GKE, which need a container runtime that native Windows lacks.
type pinned struct {
	url    map[string]string
	sha256 map[string]string
	member string
}

var tools = map[string]pinned{
	"helm": {
		url: map[string]string{
			"linux/amd64": "https://get.helm.sh/helm-v4.3.1-linux-amd64.tar.gz",
			"linux/arm64": "https://get.helm.sh/helm-v4.3.1-linux-arm64.tar.gz",
		},
		sha256: map[string]string{
			"linux/amd64": "0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b",
			"linux/arm64": "8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88",
		},
		member: "helm",
	},
	"crane": {
		url: map[string]string{
			"linux/amd64":   "https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Linux_x86_64.tar.gz",
			"linux/arm64":   "https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Linux_arm64.tar.gz",
			"windows/amd64": "https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Windows_x86_64.tar.gz",
		},
		sha256: map[string]string{
			"linux/amd64":   "0ab7a1d6932a213aed964ce97666c3077fe691c8606413674a8b3e0b9ec4cda0",
			"linux/arm64":   "898c0cff975f898a33e8c4580bdafb0e7c02c7faa33374e946762f97c4ab7110",
			"windows/amd64": "0e073ea8192c3b8442ec8aaf44d53c1050a09084669fae3a6ceb0f2026cf8b21",
		},
		member: "crane",
	},
	"ko": {
		url: map[string]string{
			"linux/amd64":   "https://github.com/ko-build/ko/releases/download/v0.19.1/ko_0.19.1_Linux_x86_64.tar.gz",
			"linux/arm64":   "https://github.com/ko-build/ko/releases/download/v0.19.1/ko_0.19.1_Linux_arm64.tar.gz",
			"windows/amd64": "https://github.com/ko-build/ko/releases/download/v0.19.1/ko_0.19.1_Windows_x86_64.tar.gz",
		},
		sha256: map[string]string{
			"linux/amd64":   "635ac6ea3fd376c935fee597fbb29ab2c2449f49ef1655085fe3aa9c25fed7a5",
			"linux/arm64":   "4099b2d1170d3b8a70e049237462efc2dd14d5fa30e9d2e5e108fb4f778cdd3f",
			"windows/amd64": "f70b0c9ea1874e81358a1886d478ebee9eb17d4f07e8dcaa779c4250a2aaea70",
		},
		member: "ko",
	},
	// Kubernetes conformance runs on GKE, so Linux only.
	"sonobuoy": {
		url: map[string]string{
			"linux/amd64": sonobuoytest.URL("linux/amd64"),
			"linux/arm64": sonobuoytest.URL("linux/arm64"),
		},
		sha256: map[string]string{
			"linux/amd64": sonobuoytest.SHA256["linux/amd64"],
			"linux/arm64": sonobuoytest.SHA256["linux/arm64"],
		},
		member: "sonobuoy",
	},
	"cloud-sql-proxy": {
		url: map[string]string{
			"linux/amd64": "https://storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/v2.27.0/cloud-sql-proxy.linux.amd64",
			"linux/arm64": "https://storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/v2.27.0/cloud-sql-proxy.linux.arm64",
		},
		sha256: map[string]string{
			"linux/amd64": "39ed2abcaa073c3b4714585e36a904e2b1d92218c5e07c18224fb73ef4bedde5",
			"linux/arm64": "8ba6ebca26cb2249b8d623fd3736d72e4b57e52bb19738515e936a01201833e5",
		},
	},
}

func cacheDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("GCPEMU_COMPAT_CACHE"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(d, "gcpemu-compat")
}

// tool returns the path of a pinned tool, downloading it once.
func tool(t *testing.T, name string) string {
	t.Helper()
	platform := goruntime.GOOS + "/" + goruntime.GOARCH
	p := tools[name]
	u, want := p.url[platform], p.sha256[platform]
	if u == "" {
		t.Skipf("no pinned %s for %s", name, platform)
	}
	bin := filepath.Join(cacheDir(t), want[:12], exe(name))
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	cl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cl.Get(u)
	if err != nil {
		missing(t, "download %s: %v", name, err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		missing(t, "download %s: %d %v", u, resp.StatusCode, err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
		t.Fatalf("%s: checksum mismatch (got %x, want %s)", u, sum, want)
	}
	if p.member != "" {
		if b, err = untarMember(b, exe(p.member)); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+".tmp", b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bin+".tmp", bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

func untarMember(archive []byte, member string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("no %s in archive: %w", member, err)
		}
		if filepath.Base(h.Name) == member && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// exe returns name with the executable suffix of this OS.
func exe(name string) string {
	if goruntime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// onPath returns a tool from PATH, or calls missing.
func onPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		missing(t, "%s not on PATH", name)
	}
	return p
}

// missing skips a test whose client toolchain is missing or fails to
// install, or fails it under GCPEMU_COMPAT_REQUIRE=1, which the Linux CI
// jobs set so that a broken setup step can't quietly drop a client (ADR
// 0025).
func missing(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("GCPEMU_COMPAT_REQUIRE") == "1" {
		t.Fatalf(format+" (GCPEMU_COMPAT_REQUIRE=1)", args...)
	}
	t.Skipf(format, args...)
}

// logVersion logs what a client's toolchain says its version is.
func logVersion(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Logf("%s %s: %v", filepath.Base(bin), strings.Join(args, " "), err)
	}
	t.Logf("%s: %s", filepath.Base(bin), bytes.TrimSpace(out))
}
