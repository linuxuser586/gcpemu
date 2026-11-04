//go:build e2e

package e2e

import (
	"archive/tar"
	"archive/zip"
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
	"path"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
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

// delete deletes kind ns/name, and fails unless the API accepts it.
func (k *kube) delete(kind, ns, name string) {
	k.t.Helper()
	if code, resp := k.do(http.MethodDelete, objPath(kind, ns, name), "", nil, nil); code/100 != 2 {
		k.t.Fatalf("delete %s %s/%s: %d %s", kind, ns, name, code, resp)
	}
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
		code, b := k.do(http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+p.Metadata.Name+"/log?tailLines=40", "", nil, nil)
		if code != http.StatusOK {
			b = fmt.Appendf(nil, "log: %d %s", code, b)
		}
		fmt.Fprintf(&out, "--- %s\n%s\n", p.Metadata.Name, b)
	}
	return out.String()
}

// diagnose returns what explains pods matching selector not being ready:
// their status, the nodes, the namespace's recent events and the logs (for
// failure diagnostics). Logs alone say nothing of a pod that never ran (#966).
func (k *kube) diagnose(ns, selector string) string {
	return k.podStatus(ns, selector) + k.nodeStatus() + k.events(ns) + k.logs(ns, selector)
}

// condition is a pod or node condition.
type condition struct{ Type, Status, Reason, Message string }

func (c condition) String() string {
	return strings.TrimSpace(c.Type + "=" + c.Status + " " + reasoned(c.Reason, c.Message))
}

// reasoned joins an API object's reason and message, either of which may be
// empty.
func reasoned(reason, message string) string {
	if reason == "" || message == "" {
		return reason + message
	}
	return reason + ": " + message
}

// containerState is a container's state, its one key being waiting, running
// or terminated.
type containerState map[string]struct {
	Reason, Message string
	ExitCode        int
}

func (s containerState) String() string {
	var out []string
	for kind, v := range s {
		line := strings.TrimSpace(kind + " " + reasoned(v.Reason, v.Message))
		if kind == "terminated" {
			line += fmt.Sprintf(" exit %d", v.ExitCode)
		}
		out = append(out, line)
	}
	return strings.Join(out, " ")
}

// containerStatus is a container's entry in a pod's status.
type containerStatus struct {
	Name             string
	RestartCount     int
	State, LastState containerState
}

func (c containerStatus) String() string {
	line := fmt.Sprintf("%s restarts=%d %s", c.Name, c.RestartCount, c.State)
	if len(c.LastState) > 0 {
		line += fmt.Sprintf(", last %s", c.LastState)
	}
	return line
}

// podStatus returns each matching pod's phase, node, unmet conditions and
// container states.
func (k *kube) podStatus(ns, selector string) string {
	var l struct {
		Items []struct {
			Metadata struct{ Name string }
			Spec     struct{ NodeName string }
			Status   struct {
				Phase, Reason, Message                   string
				Conditions                               []condition
				InitContainerStatuses, ContainerStatuses []containerStatus
			}
		}
	}
	path := "/api/v1/namespaces/" + ns + "/pods?labelSelector=" + url.QueryEscape(selector)
	if code, b := k.do(http.MethodGet, path, "", nil, &l); code != http.StatusOK {
		return fmt.Sprintf("pods: %d %s\n", code, b)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "pods %s %s:\n", ns, selector)
	for _, p := range l.Items {
		line := fmt.Sprintf("%s phase=%s node=%s %s", p.Metadata.Name, p.Status.Phase, p.Spec.NodeName, reasoned(p.Status.Reason, p.Status.Message))
		fmt.Fprintf(&out, "  %s\n", strings.TrimSpace(line))
		for _, c := range p.Status.Conditions {
			if c.Status != "True" {
				fmt.Fprintf(&out, "    %s\n", c)
			}
		}
		for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			fmt.Fprintf(&out, "    %s\n", c)
		}
	}
	return out.String()
}

// nodeStatus returns each node's readiness, its other conditions that are
// true, such as DiskPressure, and its taints.
func (k *kube) nodeStatus() string {
	var l struct {
		Items []struct {
			Metadata struct{ Name string }
			Spec     struct {
				Taints []struct{ Key, Effect string }
			}
			Status struct{ Conditions []condition }
		}
	}
	if code, b := k.do(http.MethodGet, "/api/v1/nodes", "", nil, &l); code != http.StatusOK {
		return fmt.Sprintf("nodes: %d %s\n", code, b)
	}
	var out strings.Builder
	out.WriteString("nodes:\n")
	for _, n := range l.Items {
		line := []string{n.Metadata.Name}
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" || c.Status != "False" {
				line = append(line, c.String())
			}
		}
		var taints []string
		for _, t := range n.Spec.Taints {
			taints = append(taints, t.Key+":"+t.Effect)
		}
		if len(taints) > 0 {
			line = append(line, "taints=["+strings.Join(taints, " ")+"]")
		}
		fmt.Fprintf(&out, "  %s\n", strings.Join(line, " "))
	}
	return out.String()
}

// events returns the namespace's last 30 events, oldest first.
func (k *kube) events(ns string) string {
	type event struct {
		Type, Reason, Message    string
		Count                    int
		LastTimestamp, EventTime time.Time
		InvolvedObject           struct{ Kind, Name string }
	}
	var l struct{ Items []event }
	if code, b := k.do(http.MethodGet, "/api/v1/namespaces/"+ns+"/events", "", nil, &l); code != http.StatusOK {
		return fmt.Sprintf("events: %d %s\n", code, b)
	}
	// An event carries one of the two times, by the API that recorded it.
	at := func(e event) time.Time {
		if e.LastTimestamp.IsZero() {
			return e.EventTime
		}
		return e.LastTimestamp
	}
	slices.SortFunc(l.Items, func(a, b event) int { return at(a).Compare(at(b)) })
	var out strings.Builder
	fmt.Fprintf(&out, "events %s:\n", ns)
	for _, e := range l.Items[max(0, len(l.Items)-30):] {
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf(" x%d", e.Count)
		}
		fmt.Fprintf(&out, "  %s %s %s %s/%s%s: %s\n", at(e).UTC().Format(time.TimeOnly), e.Type, e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, count, e.Message)
	}
	return out.String()
}

// ---- Istio ----

// Istio is installed from its standard Helm charts (FR-GKE-009, IF-004)
// with a pinned Helm. The charts come from the Istio release archive
// (manifests/charts), which is checksum-verified; the release's charts are
// not published to Istio's chart repositories.

// istioVersion is the pinned Istio release (charts and images).
const istioVersion = "1.31.3"

// helmVersion is the pinned Helm release; helmSHA256 its archive checksums.
const helmVersion = "v4.3.1"

var helmSHA256 = map[string]string{
	"linux-amd64":  "0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b",
	"linux-arm64":  "8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88",
	"darwin-amd64": "34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89",
	"darwin-arm64": "6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198",
	// Windows archives are .zip.
	"windows-amd64": "fcfd7b796dcf0f225abc40b5636a3b01f55f3de2fb1dfb18475df94e62c14112",
	"windows-arm64": "373edaae86db70342ef39b014e2e7764ea8f9cff08714843daa5e55b5ee492f2",
}

// helm downloads the pinned helm for the host into the cache directory
// (once), verifying its checksum.
func helm(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(cacheDir(t), "helm-"+helmVersion)
	name, ext, extract := "helm", ".tar.gz", untar
	if goruntime.GOOS == "windows" {
		name, ext, extract = "helm.exe", ".zip", unzip
	}
	bin := filepath.Join(dir, name)
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	plat := goruntime.GOOS + "-" + goruntime.GOARCH
	want, ok := helmSHA256[plat]
	if !ok {
		t.Skipf("no pinned helm for %s", plat)
	}
	asset := "helm-" + helmVersion + "-" + plat + ext
	archive := fetch(t, "https://get.helm.sh/"+asset)
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		t.Fatalf("%s: checksum mismatch (got %x, want %s)", asset, got, want)
	}
	if err := extract(archive, dir, func(n string) (string, bool) {
		return name, path.Base(n) == name
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
	// The charts are the same in every release archive; Windows takes them
	// from the Linux one rather than unpacking a .zip.
	osName, arch := goruntime.GOOS, goruntime.GOARCH
	switch osName {
	case "darwin":
		osName = "osx"
	case "windows":
		osName, arch = "linux", "amd64"
	}
	asset := fmt.Sprintf("istio-%s-%s-%s.tar.gz", istioVersion, osName, arch)
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

// unzip extracts the regular files of a .zip that pick maps to a relative
// path into dir.
func unzip(archive []byte, dir string, pick func(name string) (string, bool)) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return err
	}
	n := 0
	for _, f := range zr.File {
		rel, ok := pick(f.Name)
		if !ok || !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		if err := writeExtracted(dir, f.Name, rel, b, f.Mode()); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("nothing extracted")
	}
	return nil
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
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := writeExtracted(dir, h.Name, rel, b, os.FileMode(h.Mode)); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("nothing extracted")
	}
	return nil
}

// writeExtracted writes archive member name, picked as rel, into dir.
func writeExtracted(dir, name, rel string, b []byte, mode os.FileMode) error {
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("unsafe path %q", name)
	}
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p+".tmp", b, mode.Perm()&0o755|0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
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
	installIstioCharts(t, k,
		[]string{"istio-base", "base"},
		[]string{"istiod", "istio-control/istio-discovery"},
		[]string{"istio-ingressgateway", "gateway", "-f", valuesFile(t, gatewayValues)})
}

// installIstioCharts installs Helm releases of the Istio charts in
// istio-system, in order and each until ready; a release is its name, its
// chart's directory under manifests/charts and extra helm arguments.
func installIstioCharts(t *testing.T, k *kube, releases ...[]string) {
	t.Helper()
	bin, charts := helm(t), istioCharts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, rel := range releases {
		args := append([]string{"upgrade", "--install", rel[0], filepath.Join(charts, rel[1]), "-n", "istio-system", "--create-namespace",
			"--kubeconfig", k.file, "--wait", "--timeout", "8m"}, rel[2:]...)
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		if err != nil {
			// No selector: the release that timed out may not be istiod's.
			t.Fatalf("helm install %s: %v\n%s\n%s", rel[0], err, tail(string(out), 40), k.diagnose("istio-system", ""))
		}
	}
}

// valuesFile writes Helm values to a file and returns its path.
func valuesFile(t *testing.T, values string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "values-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(values); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}
