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
	"testing"
	"time"
)

// pinned is a downloadable tool: url and sha256 by GOARCH, and the file to
// take from a .tar.gz (empty for a plain binary).
type pinned struct {
	url    map[string]string
	sha256 map[string]string
	member string
}

var tools = map[string]pinned{
	"helm": {
		url: map[string]string{
			"amd64": "https://get.helm.sh/helm-v3.21.0-linux-amd64.tar.gz",
			"arm64": "https://get.helm.sh/helm-v3.21.0-linux-arm64.tar.gz",
		},
		sha256: map[string]string{
			"amd64": "0093eb572e3d2380f094df162ddb525e219249de88957afe24cfbb19632acd36",
			"arm64": "8de5a0c9a47431e59fd560e91e0779c8cf9316c383da7efb84128a4c339ecb2d",
		},
		member: "helm",
	},
	"crane": {
		url: map[string]string{
			"amd64": "https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Linux_x86_64.tar.gz",
			"arm64": "https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Linux_arm64.tar.gz",
		},
		sha256: map[string]string{
			"amd64": "0ab7a1d6932a213aed964ce97666c3077fe691c8606413674a8b3e0b9ec4cda0",
			"arm64": "898c0cff975f898a33e8c4580bdafb0e7c02c7faa33374e946762f97c4ab7110",
		},
		member: "crane",
	},
	"ko": {
		url: map[string]string{
			"amd64": "https://github.com/ko-build/ko/releases/download/v0.19.1/ko_0.19.1_Linux_x86_64.tar.gz",
			"arm64": "https://github.com/ko-build/ko/releases/download/v0.19.1/ko_0.19.1_Linux_arm64.tar.gz",
		},
		sha256: map[string]string{
			"amd64": "635ac6ea3fd376c935fee597fbb29ab2c2449f49ef1655085fe3aa9c25fed7a5",
			"arm64": "4099b2d1170d3b8a70e049237462efc2dd14d5fa30e9d2e5e108fb4f778cdd3f",
		},
		member: "ko",
	},
	"cloud-sql-proxy": {
		url: map[string]string{
			"amd64": "https://storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/v2.26.0/cloud-sql-proxy.linux.amd64",
			"arm64": "https://storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/v2.26.0/cloud-sql-proxy.linux.arm64",
		},
		sha256: map[string]string{
			"amd64": "a38fe97690a27490e60a7945a8a178186ff9464abb81fe2bc652a3f773443387",
			"arm64": "38a61b2d55ac5e0b3924561696f599003b50100baedbfe4730a0410713f3e2cd",
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
	p, ok := tools[name]
	if !ok || goruntime.GOOS != "linux" {
		t.Skipf("no pinned %s for %s", name, goruntime.GOOS)
	}
	u, want := p.url[goruntime.GOARCH], p.sha256[goruntime.GOARCH]
	if u == "" {
		t.Skipf("no pinned %s for %s", name, goruntime.GOARCH)
	}
	bin := filepath.Join(cacheDir(t), want[:12], name)
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	cl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cl.Get(u)
	if err != nil {
		t.Skipf("download %s: %v", name, err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("download %s: %d %v", u, resp.StatusCode, err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
		t.Fatalf("%s: checksum mismatch (got %x, want %s)", u, sum, want)
	}
	if p.member != "" {
		if b, err = untarMember(b, p.member); err != nil {
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

// onPath returns a tool from PATH or skips the test.
func onPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH", name)
	}
	return p
}
