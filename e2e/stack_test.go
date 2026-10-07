//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v5"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// TestReferenceStack runs the SRS 11.2 reference stack against an
// in-process emulator with IAM in enforce mode:
//
//  1. tofu apply creates the whole stack (stack/), and a plan right after
//     shows no changes;
//  2. a multi-arch Go API image (app/) is built and pushed to Artifact
//     Registry; Istio is installed with istioctl, its ingress gateway
//     requires mTLS from the LB, and the app is deployed; a second apply
//     adds the NEG backend service;
//  3. https://app.example.test/ serves the bucket's page, the second
//     request is a CDN hit;
//  4. /api/health goes LB → Istio → app; a client without the LB's
//     certificate is refused by the gateway;
//  5. the app writes to GCS, inserts into Cloud SQL (connector, IAM auth,
//     private IP) and publishes to Pub/Sub with Workload Identity;
//  6. bucket notification and app message arrive by push with a verified
//     OIDC token;
//  7. pod egress works through Cloud NAT and stops without it;
//  8. revoking roles/storage.objectAdmin makes step 5 fail with
//     PERMISSION_DENIED;
//  9. urlMaps.invalidateCache on /* makes the next request a miss;
//  10. tofu destroy leaves no containers.
//
// Steps 2 and 4–8 need internet access (istioctl, Istio images, step 7's
// egress target) and run only with GCPEMU_NET_TESTS=1.
func TestReferenceStack(t *testing.T) {
	emutest.RequireRuntime(t)
	tofuBin, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("OpenTofu (tofu) is not on PATH")
	}
	netTests := os.Getenv("GCPEMU_NET_TESTS") == "1"
	r := &run{t: t, begin: time.Now()}
	defer r.report()

	r.inst = emutest.Start(t, nil, emutest.WithIAMMode("enforce"))
	r.mark("emulator start")
	ctx := context.Background()
	r.compute, err = computev1.NewService(ctx, option.WithEndpoint(r.inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	r.tf = newTofu(t, tofuBin, r.inst.Endpoint("gateway"))
	r.web = r.lbClient()

	r.step("1 tofu apply", true, r.step1)
	if netTests {
		r.step("2 image istio app", true, r.step2)
	}
	r.step("3 static via cdn", true, r.step3)
	if netTests {
		r.step("4 api via istio mtls", true, r.step4)
	}
	r.step("9 invalidate cache", false, r.step9)
	if netTests {
		r.step("5 app gcs sql pubsub", false, r.step5)
		r.step("6 push oidc", false, r.step6)
		r.step("7 egress via nat", false, r.step7)
		r.step("8 iam enforce", false, r.step8)
	} else {
		t.Log("GCPEMU_NET_TESTS != 1: skipping steps 2 and 4–8 (they download istioctl and Istio images and reach the internet)")
	}
	r.step("10 tofu destroy", true, r.step10)

	total := time.Since(r.begin)
	budget := 10 * time.Minute
	if v := os.Getenv("GCPEMU_E2E_BUDGET"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			budget = d
		}
	}
	if total > budget {
		t.Errorf("reference stack took %v, budget %v (SRS 11.2)", total.Round(time.Second), budget)
	}
}

// run carries state between the steps.
type run struct {
	t       *testing.T
	inst    *emutest.Instance
	tf      *tofu
	compute *computev1.Service
	out     map[string]string
	kube    *kube
	web     *http.Client
	begin   time.Time
	last    time.Time
	times   []string
	workID  string
}

func (r *run) mark(what string) {
	now := time.Now()
	since := r.last
	if since.IsZero() {
		since = r.begin
	}
	r.times = append(r.times, fmt.Sprintf("%-34s %8v", what, now.Sub(since).Round(100*time.Millisecond)))
	r.last = now
}

// step runs fn as a subtest; a failed required step ends the test.
func (r *run) step(name string, required bool, fn func(t *testing.T)) {
	ok := r.t.Run(strings.ReplaceAll(name, " ", "_"), fn)
	r.mark("step " + name)
	if !ok && required {
		r.t.FailNow()
	}
}

func (r *run) report() {
	r.t.Logf("timings:\n  %s\n  %-34s %8v", strings.Join(r.times, "\n  "), "total", time.Since(r.begin).Round(time.Second))
}

// ---- Step 1 ----

func (r *run) step1(t *testing.T) {
	r.tf.t = t
	r.tf.run(10*time.Minute, "init", "-no-color")
	r.tf.apply(nil)
	r.tf.planClean()
	r.out = r.tf.outputs()
	if r.out["app_host"] != "app.example.test" {
		t.Fatalf("outputs: %v", r.out)
	}
}

// ---- Step 2 ----

func (r *run) step2(t *testing.T) {
	r.tf.t = t
	// The app image: multi-arch, FROM scratch, pushed to Artifact Registry.
	idx := buildAppImage(t)
	image := r.out["image_repo"] + "/api:v1"
	pushIndex(t, r.inst.Endpoint("ar"), image, r.token(t), idx)
	r.mark("  app image built and pushed")

	r.kube = newKube(t, r.inst.Endpoint("gateway"), r.out["cluster"])
	installIstio(t, r.kube)
	r.mark("  istio installed")

	// The gateway requires the LB's client certificate (MUTUAL) and serves
	// the mesh certificate the LB's trust config accepts.
	k := r.kube
	k.apply(obj("Secret", "istio-system", "gateway-mtls", nil, map[string]any{
		"type": "Opaque",
		"stringData": map[string]string{
			"tls.crt": r.out["gateway_cert_pem"], "tls.key": r.out["gateway_key_pem"], "ca.crt": r.out["lb_client_ca_pem"]},
	}))
	k.apply(obj("Gateway", "istio-system", "app", nil, map[string]any{"spec": map[string]any{
		"selector": map[string]string{"istio": "ingressgateway"},
		"servers": []any{map[string]any{
			"port":  map[string]any{"number": 443, "name": "https", "protocol": "HTTPS"},
			"hosts": []string{"*"},
			"tls":   map[string]any{"mode": "MUTUAL", "credentialName": "gateway-mtls", "minProtocolVersion": "TLSV1_2"},
		}},
	}}))
	k.apply(obj("VirtualService", "istio-system", "app", nil, map[string]any{"spec": map[string]any{
		"hosts":    []string{"*"},
		"gateways": []string{"app"},
		"http": []any{map[string]any{"route": []any{map[string]any{
			"destination": map[string]any{"host": "app.app.svc.cluster.local", "port": map[string]any{"number": 8080}}}}}},
	}}))

	r.migrate(t)

	// The app: KSA app/app bound to the app GSA (Workload Identity).
	k.apply(obj("Namespace", "", "app", nil, nil))
	k.apply(obj("ServiceAccount", "app", "app", map[string]string{"iam.gke.io/gcp-service-account": r.out["app_gsa"]}, nil))
	env := []map[string]string{}
	for k, v := range map[string]string{
		"PROJECT": r.out["project"], "BUCKET": r.out["uploads_bucket"], "TOPIC": r.out["topic"],
		"SQL_INSTANCE": r.out["sql_connection_name"], "SQL_USER": r.out["sql_iam_user"], "SQL_DB": "app",
		"PUSH_AUDIENCE": r.out["push_audience"], "PUSH_SA": r.out["push_sa"],
	} {
		env = append(env, map[string]string{"name": k, "value": v})
	}
	k.apply(obj("Deployment", "app", "app", nil, map[string]any{"spec": map[string]any{
		"replicas": 1,
		"selector": map[string]any{"matchLabels": map[string]string{"app": "app"}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]string{"app": "app"}},
			"spec": map[string]any{
				"serviceAccountName": "app",
				"containers": []any{map[string]any{
					"name": "app", "image": r.out["image_repo"] + "/api:v1", "env": env,
					"ports":          []any{map[string]any{"containerPort": 8080}},
					"readinessProbe": map[string]any{"periodSeconds": 1, "httpGet": map[string]any{"path": "/healthz", "port": 8080}},
				}},
			},
		},
	}}))
	k.apply(obj("Service", "app", "app", nil, map[string]any{"spec": map[string]any{
		"selector": map[string]string{"app": "app"},
		"ports":    []any{map[string]any{"name": "http", "port": 8080, "targetPort": 8080}},
	}}))
	eventually(t, 3*time.Minute, "app deployment ready", func() bool { return k.deploymentReady("app", "app") },
		func() string { return k.logs("app", "app=app") })
	r.mark("  app deployed")

	// The gateway's NEG now exists: add the API backend service.
	eventually(t, 2*time.Minute, "gateway NEG with an endpoint", func() bool {
		l, err := r.compute.NetworkEndpointGroups.ListNetworkEndpoints(r.out["project"], r.out["zone"], gatewayNEG,
			&computev1.NetworkEndpointGroupsListEndpointsRequest{}).Do()
		return err == nil && len(l.Items) > 0
	}, nil)
	r.tf.apply(map[string]string{"api_neg_name": gatewayNEG})
	r.tf.planClean()
}

// migrate creates the app's table as the built-in admin user (the IAM
// user has no DDL rights, like on Cloud SQL) over the private IP.
func (r *run) migrate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=5432 user=admin dbname=app sslmode=disable connect_timeout=5", r.out["sql_private_ip"]))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = "change-me-0123456789"
	var conn *pgx.Conn
	eventually(t, time.Minute, "admin connection to Cloud SQL", func() bool {
		conn, err = pgx.ConnectConfig(ctx, cfg)
		return err == nil
	}, func() string { return fmt.Sprint(err) })
	defer conn.Close(ctx)
	user := pgx.Identifier{r.out["sql_iam_user"]}.Sanitize()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS work_items (id bigserial PRIMARY KEY, item text NOT NULL, pod text NOT NULL, created_at timestamptz NOT NULL DEFAULT now())`,
		`GRANT SELECT, INSERT ON work_items TO ` + user,
		`GRANT USAGE ON SEQUENCE work_items_id_seq TO ` + user,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// ---- Step 3 ----

func (r *run) step3(t *testing.T) {
	// The managed certificate becomes ACTIVE once DNS points at the LB.
	var first *http.Response
	var body string
	eventually(t, 2*time.Minute, "https://app.example.test/", func() bool {
		first, body = r.get(t, "/")
		return first != nil && first.StatusCode == 200
	}, func() string { return body })
	if !strings.Contains(body, "gcpemu reference stack") {
		t.Fatalf("index page: %q", body)
	}
	if ct := first.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type %q", ct)
	}
	second, _ := r.get(t, "/")
	if got := second.Header.Get("X-Cache-Status"); got != "hit" {
		t.Fatalf("second request X-Cache-Status = %q (first %q), want hit", got, first.Header.Get("X-Cache-Status"))
	}
	if second.Header.Get("Age") == "" {
		t.Errorf("cached response without Age")
	}
	// The same with curl, as the SRS phrases it.
	if out, ok := r.curl(t, "/"); ok && (!strings.Contains(out, "gcpemu reference stack") || !strings.Contains(strings.ToLower(out), "x-cache-status: hit")) {
		t.Errorf("curl https://app.example.test/:\n%s", out)
	}
	// HTTP → HTTPS redirect on port 80.
	resp, err := r.web.Get("http://app.example.test/style.css?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://app.example.test/style.css?x=1" {
		t.Errorf("http:// → %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// ---- Step 4 ----

func (r *run) step4(t *testing.T) {
	var resp *http.Response
	var body string
	eventually(t, 2*time.Minute, "https://app.example.test/api/health", func() bool {
		resp, body = r.get(t, "/api/health")
		return resp != nil && resp.StatusCode == 200
	}, func() string { return body + "\n" + r.kube.logs("istio-system", "istio=ingressgateway") })
	var h struct{ Status, Pod string }
	if err := json.Unmarshal([]byte(body), &h); err != nil || h.Status != "ok" || !strings.HasPrefix(h.Pod, "app-") {
		t.Fatalf("health: %q", body)
	}
	if out, ok := r.curl(t, "/api/health"); ok && !strings.Contains(out, `"status":"ok"`) {
		t.Errorf("curl /api/health:\n%s", out)
	}

	// A client on the VPC without the LB's client certificate is refused
	// by the gateway (FR-LB-006). It trusts the mesh CA, so only the
	// missing client certificate can fail the exchange.
	ips := r.kube.podIPs("istio-system", "istio=ingressgateway")
	if len(ips) == 0 {
		t.Fatal("no ingress gateway pod")
	}
	svc, _ := r.inst.Env.Lookup("lb")
	raw, err := svc.(*lb.Service).DialBackend(context.Background(), net.JoinHostPort(ips[0], "8443"))
	if err != nil {
		t.Fatalf("dial gateway pod: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(r.out["mesh_ca_pem"]))
	tc := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: r.out["gateway_sni"]})
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(tc, "GET /api/health HTTP/1.1\r\nHost: app.example.test\r\n\r\n")
	b, err := io.ReadAll(tc)
	tc.Close()
	if err == nil && strings.Contains(string(b), " 200 ") {
		t.Fatal("the gateway accepted a client without the LB certificate")
	}
	t.Logf("direct request without client certificate refused: %v", err)
}

// ---- Steps 5–8 ----

// work calls /api/work through the load balancer.
func (r *run) work(t *testing.T) (int, map[string]any) {
	r.workID = fmt.Sprintf("w%d", time.Now().UnixNano())
	req, _ := http.NewRequest(http.MethodPost, "https://app.example.test/api/work?id="+r.workID, nil)
	resp, err := r.web.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (r *run) step5(t *testing.T) {
	code, out := r.work(t)
	if code != 200 {
		t.Fatalf("/api/work: %d %v\n%s", code, out, r.kube.logs("app", "app=app"))
	}
	// Verify each side effect in the emulator.
	ctx := context.Background()
	sc, err := storage.NewClient(ctx, option.WithEndpoint(r.inst.GatewayURL()+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if _, err := sc.Bucket(r.out["uploads_bucket"]).Object("work/" + r.workID + ".txt").Attrs(ctx); err != nil {
		t.Errorf("object written by the app: %v", err)
	}
	cfg, _ := pgx.ParseConfig(fmt.Sprintf("host=%s port=5432 user=admin dbname=app sslmode=disable connect_timeout=5", r.out["sql_private_ip"]))
	cfg.Password = "change-me-0123456789"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM work_items WHERE item = $1", r.workID).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows for %s: %d %v", r.workID, n, err)
	}
	if out["messageId"] == nil || out["messageId"] == "" {
		t.Errorf("no message ID: %v", out)
	}
}

func (r *run) step6(t *testing.T) {
	var got []struct {
		Verified   bool
		Error      string
		Email      string
		Audience   string
		Attributes map[string]string
	}
	var app, notify bool
	eventually(t, time.Minute, "push deliveries of the app message and the bucket notification", func() bool {
		resp, body := r.get(t, "/api/received")
		if resp == nil || resp.StatusCode != 200 {
			return false
		}
		_ = json.Unmarshal([]byte(body), &got)
		app, notify = false, false
		for _, d := range got {
			if !d.Verified || d.Email != r.out["push_sa"] || d.Audience != r.out["push_audience"] {
				continue
			}
			if d.Attributes["source"] == "app" && d.Attributes["id"] == r.workID {
				app = true
			}
			if d.Attributes["eventType"] == "OBJECT_FINALIZE" && d.Attributes["objectId"] == "work/"+r.workID+".txt" {
				notify = true
			}
		}
		return app && notify
	}, func() string { return fmt.Sprintf("app=%v notification=%v deliveries=%+v", app, notify, got) })
}

const egressURL = "https://example.com/"

func (r *run) egress(t *testing.T) (bool, string) {
	resp, body := r.get(t, "/api/egress?url="+egressURL)
	if resp == nil {
		return false, body
	}
	return resp.StatusCode == 200, body
}

func (r *run) step7(t *testing.T) {
	r.tf.t = t
	var body string
	eventually(t, time.Minute, "egress through Cloud NAT", func() bool {
		var ok bool
		ok, body = r.egress(t)
		return ok
	}, func() string { return body })
	r.tf.apply(map[string]string{"nat_enabled": "false"})
	eventually(t, time.Minute, "egress blocked without NAT", func() bool {
		ok, b := r.egress(t)
		body = b
		return !ok
	}, func() string { return body })
	t.Logf("without NAT: %s", strings.TrimSpace(body))
}

func (r *run) step8(t *testing.T) {
	r.tf.t = t
	r.tf.apply(map[string]string{"app_storage_access": "false"})
	code, out := r.work(t)
	if code != http.StatusForbidden || out["code"] != "PERMISSION_DENIED" || out["step"] != "gcs" {
		t.Fatalf("/api/work after revoking roles/storage.objectAdmin: %d %v", code, out)
	}
	t.Logf("denied: %v", out["error"])
}

// ---- Step 9 ----

func (r *run) step9(t *testing.T) {
	if resp, _ := r.get(t, "/"); resp == nil || resp.Header.Get("X-Cache-Status") != "hit" {
		t.Fatalf("before invalidation: want a hit")
	}
	op, err := r.compute.UrlMaps.InvalidateCache(r.out["project"], r.out["url_map"], &computev1.CacheInvalidationRule{Path: "/*"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	for op.Status != "DONE" {
		if op, err = r.compute.GlobalOperations.Wait(r.out["project"], op.Name).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if op.Error != nil {
		t.Fatalf("invalidateCache: %+v", op.Error.Errors[0])
	}
	resp, _ := r.get(t, "/")
	if got := resp.Header.Get("X-Cache-Status"); got != "miss" {
		t.Fatalf("after invalidation X-Cache-Status = %q, want miss", got)
	}
	if resp, _ = r.get(t, "/"); resp.Header.Get("X-Cache-Status") != "hit" {
		t.Errorf("refilled entry not a hit")
	}
}

// ---- Step 10 ----

func (r *run) step10(t *testing.T) {
	r.tf.t = t
	r.tf.run(15*time.Minute, "destroy", "-auto-approve")
	// `gcpemu status`: the instance owns no containers any more.
	var left []string
	eventually(t, time.Minute, "no leftover containers", func() bool {
		cts, err := r.inst.Containers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		left = left[:0]
		for _, c := range cts {
			left = append(left, c.Service+"/"+c.Role+" "+c.Name)
		}
		return len(left) == 0
	}, func() string { return strings.Join(left, "\n") })
}

// ---- helpers ----

func obj(kind, ns, name string, annotations map[string]string, extra map[string]any) map[string]any {
	api := map[string]string{"Deployment": "apps/v1", "Gateway": "networking.istio.io/v1", "VirtualService": "networking.istio.io/v1"}[kind]
	if api == "" {
		api = "v1"
	}
	md := map[string]any{"name": name}
	if ns != "" {
		md["namespace"] = ns
	}
	if annotations != nil {
		md["annotations"] = annotations
	}
	o := map[string]any{"apiVersion": api, "kind": kind, "metadata": md}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

// token returns an access token of the default principal (owner).
func (r *run) token(t *testing.T) string {
	keys, _ := r.inst.Env.Lookup("iam")
	tok, _, err := keys.(emu.ServiceAccountKeys).AccessToken(context.Background(), emu.Principal(r.inst.Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// resolver resolves through the emulated Cloud DNS.
func (r *run) resolver() *net.Resolver {
	addr := r.inst.Endpoint("dns")
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
}

// lbClient is a browser: it resolves names with the emulated DNS and
// trusts the emulator CA (FR-INT-004).
func (r *run) lbClient() *http.Client {
	res := r.resolver()
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: r.inst.Env.CA.Pool()},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, _ := net.SplitHostPort(addr)
				ips, err := res.LookupHost(ctx, host)
				if err != nil {
					return nil, err
				}
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0], port))
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// get fetches https://app.example.test<path>; resp is nil on transport
// errors (body then holds the error).
func (r *run) get(t *testing.T, path string) (*http.Response, string) {
	resp, err := r.web.Get("https://" + r.appHost() + path)
	if err != nil {
		return nil, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (r *run) appHost() string {
	if h := r.out["app_host"]; h != "" {
		return h
	}
	return "app.example.test"
}

// curl fetches path with curl (if installed), resolving the host through
// the emulated DNS and trusting the emulator CA. ok is false when curl is
// not installed.
func (r *run) curl(t *testing.T, path string) (string, bool) {
	bin, err := exec.LookPath("curl")
	if err != nil {
		return "", false
	}
	ips, err := r.resolver().LookupHost(context.Background(), r.appHost())
	if err != nil || len(ips) == 0 {
		t.Fatalf("resolve %s: %v", r.appHost(), err)
	}
	out, err := exec.Command(bin, "-sS", "-i", "--max-time", "20", "--cacert", r.inst.Env.CA.Path(),
		"--resolve", r.appHost()+":443:"+ips[0], "https://"+r.appHost()+path).CombinedOutput()
	if err != nil {
		t.Errorf("curl %s: %v\n%s", path, err, out)
	}
	return string(out), true
}

// eventually polls cond until it holds or timeout passes; diag explains a
// failure.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			msg := ""
			if diag != nil {
				msg = "\n" + diag()
			}
			t.Fatalf("timed out after %v waiting for %s%s", timeout, what, msg)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
