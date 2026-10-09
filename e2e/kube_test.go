//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// kube is a minimal Kubernetes API client (server-side apply) with the
// cluster-admin credentials of the emulator's kubeconfig.
type kube struct {
	t        *testing.T
	endpoint string
	hc       *http.Client
	file     string // kubeconfig path (for helm)
}

// newKube fetches the cluster's kubeconfig from the emulator, as
// `gcloud container clusters get-credentials` would.
func newKube(t *testing.T, gateway, cluster string) *kube {
	t.Helper()
	resp, err := http.Get("http://" + gateway + "/container/_emu/kubeconfig?cluster=" + url.QueryEscape(cluster))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kubeconfig: %d %s", resp.StatusCode, b)
	}
	var kc struct {
		Clusters []struct {
			Cluster map[string]string `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User map[string]string `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(b, &kc); err != nil || len(kc.Clusters) != 1 || len(kc.Users) != 1 {
		t.Fatalf("kubeconfig: %v %s", err, b)
	}
	dec := func(s string) []byte { v, _ := base64.StdEncoding.DecodeString(s); return v }
	cert, err := tls.X509KeyPair(dec(kc.Users[0].User["client-certificate-data"]), dec(kc.Users[0].User["client-key-data"]))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(dec(kc.Clusters[0].Cluster["certificate-authority-data"]))
	file := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return &kube{t: t, endpoint: kc.Clusters[0].Cluster["server"], file: file, hc: &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}}}}}
}

// resources maps kinds to their API paths.
var resources = map[string]struct {
	group, plural string
	namespaced    bool
}{
	"Namespace":           {"/api/v1", "namespaces", false},
	"ServiceAccount":      {"/api/v1", "serviceaccounts", true},
	"Secret":              {"/api/v1", "secrets", true},
	"Service":             {"/api/v1", "services", true},
	"Deployment":          {"/apis/apps/v1", "deployments", true},
	"Pod":                 {"/api/v1", "pods", true},
	"SecretProviderClass": {"/apis/secrets-store.csi.x-k8s.io/v1", "secretproviderclasses", true},
	"SecretSync":          {"/apis/secret-sync.gke.io/v1", "secretsyncs", true},
	"Gateway":             {"/apis/networking.istio.io/v1", "gateways", true},
	"VirtualService":      {"/apis/networking.istio.io/v1", "virtualservices", true},
}

func objPath(kind, ns, name string) string {
	r, ok := resources[kind]
	if !ok {
		panic("unknown kind " + kind)
	}
	if r.namespaced {
		return r.group + "/namespaces/" + ns + "/" + r.plural + "/" + name
	}
	return r.group + "/" + r.plural + "/" + name
}

func (k *kube) do(method, path, ct string, body []byte, out any) (int, []byte) {
	req, _ := http.NewRequest(method, k.endpoint+path, bytes.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode, b
}

// apply creates or updates obj with server-side apply.
func (k *kube) apply(obj map[string]any) {
	k.t.Helper()
	md := obj["metadata"].(map[string]any)
	ns, _ := md["namespace"].(string)
	b, _ := json.Marshal(obj)
	path := objPath(obj["kind"].(string), ns, md["name"].(string)) + "?fieldManager=gcpemu-e2e&force=true"
	// Retry briefly: CRDs and webhooks may still be settling right after
	// an install.
	var code int
	var resp []byte
	for i := 0; i < 30; i++ {
		if code, resp = k.do(http.MethodPatch, path, "application/apply-patch+yaml", b, nil); code/100 == 2 {
			return
		}
		time.Sleep(time.Second)
	}
	k.t.Fatalf("apply %s %s/%s: %d %s", obj["kind"], ns, md["name"], code, resp)
}

func (k *kube) get(path string, out any) bool {
	code, _ := k.do(http.MethodGet, path, "", nil, out)
	return code == http.StatusOK
}

// deploymentReady reports whether all replicas of a deployment are
// updated and available.
func (k *kube) deploymentReady(ns, name string) bool {
	var d struct {
		Spec   struct{ Replicas int }
		Status struct {
			UpdatedReplicas, AvailableReplicas, ReadyReplicas int
		}
	}
	if !k.get(objPath("Deployment", ns, name), &d) {
		return false
	}
	return d.Status.AvailableReplicas >= d.Spec.Replicas && d.Status.UpdatedReplicas >= d.Spec.Replicas && d.Spec.Replicas > 0
}

// podIPs returns the IPs of running pods matching a label selector.
func (k *kube) podIPs(ns, selector string) []string {
	var l struct {
		Items []struct {
			Status struct {
				Phase string
				PodIP string `json:"podIP"`
			}
		}
	}
	k.get("/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape(selector), &l)
	var out []string
	for _, p := range l.Items {
		if p.Status.Phase == "Running" && p.Status.PodIP != "" {
			out = append(out, p.Status.PodIP)
		}
	}
	return out
}

// logs returns the last lines of the first pod matching selector (for
// failure diagnostics).
func (k *kube) logs(ns, selector string) string {
	var l struct {
		Items []struct{ Metadata struct{ Name string } }
	}
	k.get("/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape(selector), &l)
	var out strings.Builder
	for _, p := range l.Items {
		_, b := k.do(http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+p.Metadata.Name+"/log?tailLines=40", "", nil, nil)
		fmt.Fprintf(&out, "--- %s\n%s\n", p.Metadata.Name, b)
	}
	return out.String()
}

// ---- Istio ----

// Istio is installed from its standard Helm charts (FR-GKE-009, IF-004)
// with a pinned Helm. The charts come from the Istio release archive
// (manifests/charts), which is checksum-verified; the release's charts are
// not published to Istio's chart repositories.

// istioVersion is the pinned Istio release (charts and images).
const istioVersion = "1.31.1"

// helmVersion is the pinned Helm release; helmSHA256 its archive checksums.
const helmVersion = "v3.21.0"

var helmSHA256 = map[string]string{
	"linux-amd64":  "0093eb572e3d2380f094df162ddb525e219249de88957afe24cfbb19632acd36",
	"linux-arm64":  "8de5a0c9a47431e59fd560e91e0779c8cf9316c383da7efb84128a4c339ecb2d",
	"darwin-amd64": "8bc0c1f85f8738cc3cda4a2cc73047145bcdcb1f4d9cdcc29073037bfb22fa2e",
	"darwin-arm64": "68bfbdc022c543a2a022597b20298216877e98abe6e4a345d3ecf114d79cae5f",
}

// helm downloads the pinned helm for the host into the cache directory
// (once), verifying its checksum.
func helm(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(cacheDir(t), "helm-"+helmVersion)
	bin := filepath.Join(dir, "helm")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	plat := goruntime.GOOS + "-" + goruntime.GOARCH
	want, ok := helmSHA256[plat]
	if !ok {
		t.Skipf("no pinned helm for %s", plat)
	}
	asset := "helm-" + helmVersion + "-" + plat + ".tar.gz"
	archive := fetch(t, "https://get.helm.sh/"+asset)
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		t.Fatalf("%s: checksum mismatch (got %x, want %s)", asset, got, want)
	}
	if err := untar(archive, dir, func(name string) (string, bool) {
		return "helm", filepath.Base(name) == "helm"
	}); err != nil {
		t.Fatalf("%s: %v", asset, err)
	}
	return bin
}

// istioCharts downloads the pinned Istio release for the host into the
// cache directory (once), verifying the release checksum, and returns its
// manifests/charts directory.
func istioCharts(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(cacheDir(t), "istio-"+istioVersion, "charts")
	if _, err := os.Stat(filepath.Join(dir, "gateway", "Chart.yaml")); err == nil {
		return dir
	}
	osName := goruntime.GOOS
	if osName == "darwin" {
		osName = "osx"
	}
	asset := fmt.Sprintf("istio-%s-%s-%s.tar.gz", istioVersion, osName, goruntime.GOARCH)
	base := "https://github.com/istio/istio/releases/download/" + istioVersion + "/"
	archive := fetch(t, base+asset)
	sum := strings.Fields(string(fetch(t, base+asset+".sha256")))
	got := sha256.Sum256(archive)
	if len(sum) == 0 || sum[0] != hex.EncodeToString(got[:]) {
		t.Fatalf("%s: checksum mismatch (got %x, release says %v)", asset, got, sum)
	}
	prefix := "istio-" + istioVersion + "/manifests/charts/"
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := untar(archive, tmp, func(name string) (string, bool) {
		rel, ok := strings.CutPrefix(name, prefix)
		return rel, ok && rel != ""
	}); err != nil {
		t.Fatalf("%s: %v", asset, err)
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// untar extracts the regular files of a .tar.gz that pick maps to a
// relative path into dir.
func untar(archive []byte, dir string, pick func(name string) (string, bool)) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, ok := pick(h.Name)
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		rel = filepath.Clean(rel)
		if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p+".tmp", b, os.FileMode(h.Mode)&0o755|0o600); err != nil {
			return err
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("nothing extracted")
	}
	return nil
}

func fetch(t *testing.T, u string) []byte {
	t.Helper()
	cl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cl.Get(u)
	if err != nil {
		t.Fatalf("download %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("download %s: %d %v", u, resp.StatusCode, err)
	}
	return b
}

// gatewayNEG is the standalone NEG of the ingress gateway's HTTPS port.
const gatewayNEG = "ref-istio-gateway"

// gatewayValues configures the ingress gateway chart: its Service exposes
// 443 → 8443 as a GKE standalone NEG for the load balancer
// (container-native load balancing).
const gatewayValues = `service:
  type: ClusterIP
  annotations:
    cloud.google.com/neg: '{"exposed_ports":{"443":{"name":"` + gatewayNEG + `"}}}'
  ports:
  - name: status-port
    port: 15021
    protocol: TCP
    targetPort: 15021
  - name: https
    port: 443
    protocol: TCP
    targetPort: 8443
`

// installIstio installs Istio's base, istiod and gateway Helm charts.
func installIstio(t *testing.T, k *kube) {
	t.Helper()
	bin, charts := helm(t), istioCharts(t)
	values := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(values, []byte(gatewayValues), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, rel := range [][]string{
		{"istio-base", "base"},
		{"istiod", "istio-control/istio-discovery"},
		{"istio-ingressgateway", "gateway", "-f", values},
	} {
		args := append([]string{"upgrade", "--install", rel[0], filepath.Join(charts, rel[1]), "-n", "istio-system", "--create-namespace",
			"--kubeconfig", k.file, "--wait", "--timeout", "8m"}, rel[2:]...)
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm install %s: %v\n%s\n%s", rel[0], err, tail(string(out), 40), k.logs("istio-system", "app=istiod"))
		}
	}
}
