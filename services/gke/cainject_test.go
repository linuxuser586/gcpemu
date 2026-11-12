package gke

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// parsePod decodes an admitted pod.
func parsePod(t *testing.T, s string) *admitPod {
	t.Helper()
	var p admitPod
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// renderPatch encodes a JSON patch.
func renderPatch(ops []patchOp) string {
	b, _ := json.Marshal(ops)
	return string(b)
}

func TestInjectPatch(t *testing.T) {
	// A bare pod gets the volume, both mounts and every variable.
	ops := injectPatch(parsePod(t, `{"spec":{"containers":[{"name":"a"}],"initContainers":[{"name":"i","env":[{"name":"X"}]}]}}`), "ns")
	got := renderPatch(ops)
	for _, want := range []string{
		`{"op":"add","path":"/spec/volumes","value":[{"configMap":{"items":[{"key":"ca-certificates.crt","path":"ca-certificates.crt"}],"name":"gcpemu-ca-bundle","optional":true},"name":"gcpemu-ca-bundle"}]}`,
		`"path":"/spec/containers/0/volumeMounts","value":[{"mountPath":"/etc/gcpemu/certs","name":"gcpemu-ca-bundle","readOnly":true},{"mountPath":"/etc/ssl/certs/ca-certificates.crt","name":"gcpemu-ca-bundle","readOnly":true,"subPath":"ca-certificates.crt"}]`,
		`"path":"/spec/containers/0/env","value":[{"name":"SSL_CERT_FILE","value":"/etc/gcpemu/certs/ca-certificates.crt"}`,
		`{"name":"GCE_METADATA_HOST","value":"169.254.169.254"}`,
		`{"op":"add","path":"/spec/initContainers/0/env/-","value":{"name":"SSL_CERT_FILE"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("patch lacks %s\n%s", want, got)
		}
	}

	// Existing mounts and variables are respected.
	got = renderPatch(injectPatch(parsePod(t, `{"spec":{"volumes":[{"name":"v"}],"containers":[{"name":"a",
		"volumeMounts":[{"name":"v","mountPath":"/etc/ssl/certs"}],"env":[{"name":"SSL_CERT_FILE"}]}]}}`), "ns"))
	if strings.Contains(got, `"/etc/ssl/certs/ca-certificates.crt"`) || strings.Contains(got, `"SSL_CERT_FILE"`) ||
		!strings.Contains(got, `"path":"/spec/volumes/-"`) || !strings.Contains(got, `"REQUESTS_CA_BUNDLE"`) {
		t.Errorf("patch = %s", got)
	}

	// Opt-out and idempotence.
	if ops := injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/inject":"disabled"}},"spec":{"containers":[{}]}}`), "ns"); ops != nil {
		t.Errorf("opted-out pod patched: %v", ops)
	}
	if ops := injectPatch(parsePod(t, `{"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`), "ns"); ops != nil {
		t.Errorf("injected pod patched again: %v", ops)
	}
}

func TestInjectPatchRealGoogleAPIs(t *testing.T) {
	// A labelled pod resolves through the node's googleapis resolver,
	// keeping the cluster's search path.
	got := renderPatch(injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"containers":[{}]}}`), "ci"))
	for _, want := range []string{
		`{"op":"add","path":"/spec/dnsPolicy","value":"None"}`,
		`{"op":"add","path":"/spec/dnsConfig","value":{"nameservers":["169.254.169.253"],"options":[{"name":"ndots","value":"5"}],"searches":["ci.svc.cluster.local","svc.cluster.local","cluster.local"]}}`,
		`"SSL_CERT_FILE"`, // still trusts the CA, for LOCATION-docker.pkg.dev
	} {
		if !strings.Contains(got, want) {
			t.Errorf("patch lacks %s\n%s", want, got)
		}
	}

	// Already injected (reinvocation): only the DNS settings are added.
	got = renderPatch(injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`), "ci"))
	if !strings.Contains(got, `"/spec/dnsPolicy"`) || strings.Contains(got, `"/spec/volumes`) {
		t.Errorf("patch = %s", got)
	}

	// A pod with its own DNS (or patched already), or without the label,
	// is left alone.
	for _, pod := range []string{
		`{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"dnsPolicy":"None","volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`,
		`{"metadata":{"labels":{"gcpemu.dev/googleapis":"emulated"}},"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`,
	} {
		if ops := injectPatch(parsePod(t, pod), "ci"); ops != nil {
			t.Errorf("%s patched: %s", pod, renderPatch(ops))
		}
	}

	// A dnsConfig of the pod's own is kept after the resolver's settings.
	got = renderPatch(injectPatch(parsePod(t, `{"metadata":{"labels":{"gcpemu.dev/googleapis":"real"}},"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],
		"dnsConfig":{"nameservers":["10.0.0.53"],"searches":["corp.internal"],"options":[{"name":"ndots","value":"2"},{"name":"edns0"}]},"containers":[{}]}}`), "ci"))
	if want := `{"op":"add","path":"/spec/dnsConfig","value":{"nameservers":["169.254.169.253","10.0.0.53"],"options":[{"name":"ndots","value":"2"},{"name":"edns0"}],"searches":["ci.svc.cluster.local","svc.cluster.local","cluster.local","corp.internal"]}}`; !strings.Contains(got, want) {
		t.Errorf("patch lacks %s\n%s", want, got)
	}
}

func TestNodeDNSAnswer(t *testing.T) {
	ip := net.ParseIP(metadataIP)
	ans := nodeDNSAnswer(nodeConfig{Frontend: "10.0.0.1:443", FrontendHosts: []string{"storage.googleapis.com"}}, ip)
	for name, want := range map[string]bool{
		"metadata.google.internal": true, "storage.googleapis.com": true, "us-docker.pkg.dev": true,
		"logging.googleapis.com": false, "example.com": false,
	} {
		if got := ans(name) != nil; got != want {
			t.Errorf("%s answered = %v, want %v", name, got, want)
		}
	}
	// Without a frontend only the metadata names are answered.
	ans = nodeDNSAnswer(nodeConfig{FrontendHosts: []string{"storage.googleapis.com"}}, ip)
	if ans("storage.googleapis.com") != nil || ans("metadata.google.internal") == nil {
		t.Error("frontend names answered without a frontend")
	}
}

func TestClusterDNS(t *testing.T) {
	// k3s gives kube-dns the 10th address of the service range.
	if got, err := clusterDNS("34.118.224.0/20"); err != nil || got != "34.118.224.10" {
		t.Errorf("clusterDNS = %q, %v", got, err)
	}
	if _, err := clusterDNS(""); err == nil {
		t.Error("clusterDNS accepted no range")
	}
}

func TestGoogleAPIsDNS(t *testing.T) {
	// Each upstream answers every A query with its own address and
	// records the VPC network the query carried.
	upstream := func(ip string) (string, *atomic.Value) {
		t.Helper()
		l, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var network atomic.Value
		srv := &mdns.Server{PacketConn: l, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, r *mdns.Msg) {
			network.Store(frontend.Network(r))
			m := new(mdns.Msg)
			m.SetReply(r)
			m.Answer = append(m.Answer, &mdns.A{Hdr: mdns.RR_Header{Name: r.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 5}, A: net.ParseIP(ip)})
			_ = w.WriteMsg(m)
		})}
		go func() { _ = srv.ActivateAndServe() }()
		t.Cleanup(func() { _ = srv.Shutdown() })
		return l.LocalAddr().String(), &network
	}
	cloud, cloudNet := upstream("192.0.2.1")
	cluster, clusterNet := upstream("192.0.2.2")

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &mdns.Server{PacketConn: pc, Handler: googleAPIsDNS(cloud, cluster, "projects/p/global/networks/n")}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	ask := func(name string) string {
		t.Helper()
		m := new(mdns.Msg)
		m.SetQuestion(name, mdns.TypeA)
		c := &mdns.Client{}
		for i := 0; i < 20; i++ { // server start race
			if r, _, err := c.Exchange(m, pc.LocalAddr().String()); err == nil && len(r.Answer) == 1 {
				return r.Answer[0].(*mdns.A).A.String()
			}
		}
		t.Fatalf("query %s failed", name)
		return ""
	}
	// Google APIs resolve through Cloud DNS, on the node's network, as on GCP.
	for _, name := range []string{"storage.googleapis.com.", "GoogleAPIs.com."} {
		if got := ask(name); got != "192.0.2.1" {
			t.Errorf("%s = %s, want Cloud DNS's answer", name, got)
		}
	}
	if got := cloudNet.Load(); got != "projects/p/global/networks/n" {
		t.Errorf("Cloud DNS query network = %v", got)
	}
	// Everything else, Artifact Registry and search-path expansions of
	// Google APIs included, resolves through the cluster's DNS.
	for _, name := range []string{"kubernetes.default.svc.cluster.local.", "us-docker.pkg.dev.",
		"storage.googleapis.com.ci.svc.cluster.local.", "example.com."} {
		if got := ask(name); got != "192.0.2.2" {
			t.Errorf("%s = %s, want the cluster DNS's answer", name, got)
		}
	}
	if got := clusterNet.Load(); got != "" {
		t.Errorf("cluster DNS query network = %v", got)
	}
}

// caInjectorHook is the webhook caInjectorConfig registers.
func caInjectorHook() map[string]any {
	return caInjectorConfig("https://h/admit", []byte("ca"))["webhooks"].([]map[string]any)[0]
}

// The injector fails closed: a pod the API server can't get admitted by
// the webhook is refused, not started without the Instance CA (#1053).
func TestCAInjectorConfigFailsClosed(t *testing.T) {
	hook := caInjectorHook()
	if got := hook["failurePolicy"]; got != "Fail" {
		t.Errorf("failurePolicy = %v, want Fail", got)
	}
	if got := hook["timeoutSeconds"]; got != 10 {
		t.Errorf("timeoutSeconds = %v, want 10", got)
	}
}

// admitHarness serves pod admissions for one cluster whose API server is a
// fakeKube that counts requests and, while failing is set, answers 500.
type admitHarness struct {
	t       *testing.T
	s       *Service
	ca      *ca.CA
	kube    *fakeKube
	calls   atomic.Int32
	failing atomic.Bool
	now     time.Time
	// beforePatch, when set, runs (under the fake's lock) before a PATCH
	// reaches the fake: a writer racing the admission.
	beforePatch func()
}

const admitKey = "p/us-central1-a/c1"

func newAdmitHarness(t *testing.T) *admitHarness {
	t.Helper()
	h := &admitHarness{t: t, s: newUnitService(t), now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.s.bundleNow = func() time.Time { return h.now }
	var err error
	if h.ca, err = ca.Load(t.TempDir(), "unit"); err != nil {
		t.Fatal(err)
	}
	h.s.env.CA = h.ca
	h.kube = &fakeKube{t: t, objs: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		if h.failing.Load() {
			http.Error(w, "etcdserver: request timed out", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPatch && h.beforePatch != nil {
			h.kube.mu.Lock()
			h.beforePatch()
			h.kube.mu.Unlock()
		}
		h.kube.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h.s.rtFor(admitKey).kube = &kubeClient{base: srv.URL, hc: srv.Client()}
	return h
}

// admit sends the CREATE of pod in ns and returns the response.
func (h *admitHarness) admit(ctx context.Context, ns, pod string) *admissionResponse {
	h.t.Helper()
	return h.admitReq(ctx, &admissionRequest{UID: "u1", Namespace: ns, Operation: "CREATE", Object: json.RawMessage(pod)})
}

// admitReq sends req and returns the response.
func (h *admitHarness) admitReq(ctx context.Context, req *admissionRequest) *admissionResponse {
	h.t.Helper()
	b, _ := json.Marshal(admissionReview{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview", Request: req})
	w := httptest.NewRecorder()
	h.s.servePodAdmission(w, httptest.NewRequest(http.MethodPost, "/admit", bytes.NewReader(b)).WithContext(ctx), admitKey)
	var ar admissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &ar); err != nil || ar.Response == nil {
		h.t.Fatalf("admission answer %q: %v", w.Body.String(), err)
	}
	return ar.Response
}

const plainPod = `{"spec":{"containers":[{"name":"a"}]}}`

// A pod whose namespace's bundle can't be ensured is refused, saying why,
// so its controller retries rather than starting it without the Instance CA.
func TestPodAdmissionRefusedWithoutBundle(t *testing.T) {
	h := newAdmitHarness(t)
	h.failing.Store(true)
	resp := h.admit(context.Background(), "web", plainPod)
	if resp.Allowed || resp.Patch != "" {
		t.Fatalf("admitted with the API server failing: %+v", resp)
	}
	if resp.Status == nil || !strings.Contains(resp.Status.Message, "gcpemu-ca-bundle") || !strings.Contains(resp.Status.Message, "timed out") || resp.Status.Code != 503 {
		t.Errorf("status = %+v, want 503 naming the gcpemu-ca-bundle ConfigMap and the cause", resp.Status)
	}

	// The request's context ending (the API server gave up) refuses too.
	h.failing.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if resp := h.admit(ctx, "web", plainPod); resp.Allowed || resp.Status == nil || !strings.Contains(resp.Status.Message, "gcpemu-ca-bundle") {
		t.Errorf("admitted after the request ended: %+v", resp)
	}

	// A dry run still gets the patch without the bundle.
	h.failing.Store(true)
	dry := true
	if resp := h.admitReq(context.Background(), &admissionRequest{UID: "u2", Namespace: "web", Operation: "CREATE",
		DryRun: &dry, Object: json.RawMessage(plainPod)}); !resp.Allowed || resp.Patch == "" {
		t.Errorf("dry run answer = %+v", resp)
	}
}

// A namespace's bundle confirmed current is not read again for
// caBundleTTL, so a busy API server isn't asked during every admission
// (#1053).
func TestPodAdmissionCachesBundle(t *testing.T) {
	h := newAdmitHarness(t)
	ctx := context.Background()
	if resp := h.admit(ctx, "web", plainPod); !resp.Allowed || resp.Patch == "" {
		t.Fatalf("first admission = %+v", resp)
	}
	if h.kube.get("/api/v1/namespaces/web/configmaps/gcpemu-ca-bundle") == nil {
		t.Fatal("bundle ConfigMap not created")
	}

	h.calls.Store(0)
	h.now = h.now.Add(caBundleTTL - time.Second)
	if resp := h.admit(ctx, "web", plainPod); !resp.Allowed || resp.Patch == "" {
		t.Fatalf("cached admission = %+v", resp)
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("admission within caBundleTTL made %d API server requests, want 0", n)
	}

	// Another namespace is checked on its own.
	h.admit(ctx, "jobs", plainPod)
	if n := h.calls.Load(); n == 0 {
		t.Error("first admission in another namespace made no API server request")
	}

	// After caBundleTTL the ConfigMap is read again, and a missing one recreated.
	h.calls.Store(0)
	h.kube.remove("/api/v1/namespaces/web/configmaps/gcpemu-ca-bundle")
	h.now = h.now.Add(2 * time.Second) // caBundleTTL + 1s since it was confirmed
	h.admit(ctx, "web", plainPod)
	if n := h.calls.Load(); n == 0 {
		t.Error("admission after caBundleTTL made no API server request")
	}
	if h.kube.get("/api/v1/namespaces/web/configmaps/gcpemu-ca-bundle") == nil {
		t.Error("deleted bundle ConfigMap not recreated after caBundleTTL")
	}
}

// A rotated Instance CA misses the cache: the next admission refreshes
// the namespace's ConfigMap to hold the new CA at once.
func TestPodAdmissionRefreshesBundleAfterCARotation(t *testing.T) {
	h := newAdmitHarness(t)
	ctx := context.Background()
	h.admit(ctx, "web", plainPod)
	const path = "/api/v1/namespaces/web/configmaps/gcpemu-ca-bundle"
	bundle := func() string {
		data, _ := h.kube.get(path)["data"].(map[string]any)
		b, _ := data[caBundleKey].(string)
		return b
	}
	old := bundle()
	if !strings.Contains(old, strings.TrimSpace(string(h.ca.PEM()))) {
		t.Fatal("bundle ConfigMap lacks the Instance CA")
	}

	if err := h.ca.Rotate("unit"); err != nil {
		t.Fatal(err)
	}
	h.calls.Store(0)
	h.now = h.now.Add(time.Second)
	if resp := h.admit(ctx, "web", plainPod); !resp.Allowed {
		t.Fatalf("admission after rotation = %+v", resp)
	}
	if h.calls.Load() == 0 {
		t.Fatal("admission after rotation made no API server request")
	}
	got := bundle()
	if got == old || !strings.Contains(got, strings.TrimSpace(string(h.ca.PEM()))) {
		t.Error("bundle ConfigMap not refreshed to the rotated Instance CA")
	}
}

// The ways out are unchanged and cost no API server request: kube-system,
// and gcpemu.dev/inject=disabled on the pod or, through the webhook's
// namespaceSelector, its namespace.
func TestPodAdmissionOptOutsMakeNoRequest(t *testing.T) {
	h := newAdmitHarness(t)
	h.failing.Store(true) // any request would refuse the pod
	for _, c := range []struct{ ns, pod string }{
		{"kube-system", plainPod},
		{"web", `{"metadata":{"labels":{"gcpemu.dev/inject":"disabled"}},"spec":{"containers":[{"name":"a"}]}}`},
	} {
		if resp := h.admit(context.Background(), c.ns, c.pod); !resp.Allowed || resp.Patch != "" {
			t.Errorf("%s %s: %+v, want admitted unchanged", c.ns, c.pod, resp)
		}
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("opted-out admissions made %d API server requests", n)
	}

	hook := caInjectorHook()
	sel, _ := json.Marshal(hook["namespaceSelector"])
	for _, want := range []string{`"values":["kube-system"]`, `{"key":"gcpemu.dev/inject","operator":"NotIn","values":["disabled"]}`} {
		if !strings.Contains(string(sel), want) {
			t.Errorf("namespaceSelector %s lacks %s", sel, want)
		}
	}
}

// A refresh doesn't overwrite a ConfigMap another writer changed since it
// was read: after a rotation, an admission still holding the old Instance
// CA could otherwise put it back and a cache entry for the new one would
// stand over it for up to caBundleTTL. The losing admission refuses its
// pod, which its controller retries.
func TestPodAdmissionRefreshIsConditional(t *testing.T) {
	h := newAdmitHarness(t)
	const path = "/api/v1/namespaces/web/configmaps/gcpemu-ca-bundle"
	h.kube.put(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"web","name":"gcpemu-ca-bundle","resourceVersion":"1"},"data":{"ca-certificates.crt":"stale"}}`)
	h.beforePatch = func() {
		cm := h.kube.objs[path]
		cm["metadata"].(map[string]any)["resourceVersion"] = "2"
		cm["data"] = map[string]any{caBundleKey: "written concurrently"}
	}
	if resp := h.admit(context.Background(), "web", plainPod); resp.Allowed {
		t.Errorf("admitted over a concurrent write: %+v", resp)
	}
	if data, _ := h.kube.get(path)["data"].(map[string]any); data[caBundleKey] != "written concurrently" {
		t.Errorf("concurrent write overwritten: %v", data[caBundleKey])
	}

	// With no other writer, the next admission refreshes it.
	h.beforePatch = nil
	if resp := h.admit(context.Background(), "web", plainPod); !resp.Allowed {
		t.Errorf("retry refused: %+v", resp)
	}
}
