//go:build compat

package compat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const project = "compat-proj"

// emulator is a detached gcpemu instance and the environment clients run in.
type emulator struct {
	bin, dir string
	vars     map[string]string // from `gcpemu env`
	env      []string          // full client environment
	runtime  bool              // a container runtime is available
}

// start builds gcpemu statically and starts a detached instance.
func start(t *testing.T) *emulator {
	t.Helper()
	tmp := t.TempDir()
	e := &emulator{bin: filepath.Join(tmp, "gcpemu"), dir: filepath.Join(tmp, "data")}
	build := exec.Command("go", "build", "-o", e.bin, "../cmd/gcpemu")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	services := "iam,gcs,pubsub,secrets,dns,ar,compute,lb,certs,cdn"
	if exec.Command("docker", "info").Run() == nil {
		e.runtime = true
		services += ",sql,gke"
	}
	cmd := exec.Command(e.bin, "start", "--detach", "--data-dir", e.dir, "--ephemeral", "--port", "0",
		"--services", services, "--wait-timeout", "120s")
	cmd.Env = append(os.Environ(), "CI=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gcpemu start: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command(e.bin, "stop", "--data-dir", e.dir).Run()
	})
	e.vars = e.emuEnv(t)
	token := filepath.Join(tmp, "token")
	_ = os.WriteFile(token, []byte("owner"), 0o600) // the emulator's default-principal token
	e.vars["KUBECONFIG"] = filepath.Join(tmp, "kubeconfig")
	for k, v := range map[string]string{
		"CLOUDSDK_CONFIG":                                 filepath.Join(tmp, "gcloud"),
		"CLOUDSDK_CORE_PROJECT":                           project,
		"CLOUDSDK_CORE_DISABLE_PROMPTS":                   "1",
		"CLOUDSDK_CORE_DISABLE_USAGE_REPORTING":           "1",
		"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK": "1",
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE":                 token,
		"DOCKER_CONFIG":                                   filepath.Join(tmp, "docker"),
		"HELM_CACHE_HOME":                                 filepath.Join(tmp, "helm", "cache"),
		"HELM_CONFIG_HOME":                                filepath.Join(tmp, "helm", "config"),
		"HELM_DATA_HOME":                                  filepath.Join(tmp, "helm", "data"),
	} {
		e.vars[k] = v
	}
	_ = os.MkdirAll(e.vars["DOCKER_CONFIG"], 0o700)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); e.vars[k] == "" {
			e.env = append(e.env, kv)
		}
	}
	for k, v := range e.vars {
		e.env = append(e.env, k+"="+v)
	}
	return e
}

// emuEnv returns the variables `gcpemu env` prints now (it grows as
// resources such as Cloud SQL instances appear).
func (e *emulator) emuEnv(t *testing.T) map[string]string {
	t.Helper()
	out, err := exec.Command(e.bin, "env", "--data-dir", e.dir).Output()
	if err != nil {
		t.Fatalf("gcpemu env: %v", err)
	}
	vars := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "export "), "=")
		if ok {
			vars[k] = strings.Trim(v, "'")
		}
	}
	return vars
}

// run runs a client and fails the test on error.
func (e *emulator) run(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := e.try(bin, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return out
}

func (e *emulator) try(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func contains(t *testing.T, what, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("%s: %q not in output:\n%s", what, want, out)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestClients runs every client against one emulator. Subtests share
// resources (the SQL instance, the cluster) and run in order.
func TestClients(t *testing.T) {
	e := start(t)
	gcloud := func(t *testing.T) string { return onPath(t, "gcloud") }

	t.Run("gcloud/storage", func(t *testing.T) {
		g := gcloud(t)
		f := filepath.Join(t.TempDir(), "hello.txt")
		_ = os.WriteFile(f, []byte("hello compat\n"), 0o644)
		e.run(t, g, "storage", "buckets", "create", "gs://compat-bucket")
		e.run(t, g, "storage", "cp", f, "gs://compat-bucket/dir/hello.txt")
		contains(t, "storage ls", e.run(t, g, "storage", "ls", "gs://compat-bucket/dir/"), "gs://compat-bucket/dir/hello.txt")
		contains(t, "storage cat", e.run(t, g, "storage", "cat", "gs://compat-bucket/dir/hello.txt"), "hello compat")
		contains(t, "objects describe", e.run(t, g, "storage", "objects", "describe", "gs://compat-bucket/dir/hello.txt", "--format=value(size)"), "13")
		dst := filepath.Join(t.TempDir(), "back.txt")
		e.run(t, g, "storage", "cp", "gs://compat-bucket/dir/hello.txt", dst)
		if b, _ := os.ReadFile(dst); string(b) != "hello compat\n" {
			t.Fatalf("downloaded %q", b)
		}
		e.run(t, g, "storage", "rm", "gs://compat-bucket/dir/hello.txt")
		if out, _ := e.try(g, "storage", "ls", "gs://compat-bucket/dir/"); strings.Contains(out, "hello.txt") {
			t.Fatalf("object still listed: %s", out)
		}
	})

	t.Run("gcloud/pubsub", func(t *testing.T) {
		g := gcloud(t)
		e.run(t, g, "pubsub", "topics", "create", "compat-topic")
		e.run(t, g, "pubsub", "subscriptions", "create", "compat-sub", "--topic", "compat-topic")
		e.run(t, g, "pubsub", "topics", "publish", "compat-topic", "--message", "hi from gcloud", "--attribute", "k=v")
		var out string
		for i := 0; i < 10 && !strings.Contains(out, "hi from gcloud"); i++ {
			out = e.run(t, g, "pubsub", "subscriptions", "pull", "compat-sub", "--auto-ack", "--format=value(message.data,message.attributes)")
		}
		contains(t, "pull", out, "hi from gcloud")
	})

	t.Run("gcloud/secrets", func(t *testing.T) {
		g := gcloud(t)
		data := filepath.Join(t.TempDir(), "secret.txt")
		if err := os.WriteFile(data, []byte("from gcloud"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.run(t, g, "secrets", "create", "compat-secret", "--replication-policy=automatic", "--labels=env=compat", "--data-file="+data)
		e.run(t, g, "secrets", "versions", "add", "compat-secret", "--data-file="+data)
		contains(t, "versions access", e.run(t, g, "secrets", "versions", "access", "latest", "--secret=compat-secret"), "from gcloud")
		e.run(t, g, "secrets", "versions", "disable", "1", "--secret=compat-secret")
		contains(t, "versions list", e.run(t, g, "secrets", "versions", "list", "compat-secret", "--format=value(name,state)"), "disabled")
		e.run(t, g, "secrets", "add-iam-policy-binding", "compat-secret", "--member=user:reader@example.com", "--role=roles/secretmanager.secretAccessor")
		contains(t, "get-iam-policy", e.run(t, g, "secrets", "get-iam-policy", "compat-secret"), "secretAccessor")
		e.run(t, g, "secrets", "create", "compat-regional", "--location=us-central1", "--data-file="+data)
		contains(t, "regional access", e.run(t, g, "secrets", "versions", "access", "latest", "--secret=compat-regional", "--location=us-central1"), "from gcloud")
		contains(t, "secrets list", e.run(t, g, "secrets", "list", "--format=value(name)"), "compat-secret")
		e.run(t, g, "secrets", "delete", "compat-secret", "--quiet")
	})

	t.Run("gcloud/iam", func(t *testing.T) {
		g := gcloud(t)
		e.run(t, g, "iam", "service-accounts", "create", "compat-sa", "--display-name", "compat")
		sa := "compat-sa@" + project + ".iam.gserviceaccount.com"
		contains(t, "service-accounts list", e.run(t, g, "iam", "service-accounts", "list", "--format=value(email)"), sa)
		key := filepath.Join(t.TempDir(), "key.json")
		e.run(t, g, "iam", "service-accounts", "keys", "create", key, "--iam-account", sa)
		var k struct {
			Type        string `json:"type"`
			ClientEmail string `json:"client_email"`
		}
		b, _ := os.ReadFile(key)
		if json.Unmarshal(b, &k) != nil || k.Type != "service_account" || k.ClientEmail != sa {
			t.Fatalf("key file: %s", b)
		}
		e.run(t, g, "projects", "add-iam-policy-binding", project, "--member", "serviceAccount:"+sa, "--role", "roles/storage.objectViewer")
		contains(t, "get-iam-policy", e.run(t, g, "projects", "get-iam-policy", project, "--format=json"), "serviceAccount:"+sa)
		contains(t, "print-access-token (impersonated)",
			e.run(t, g, "auth", "print-access-token", "--impersonate-service-account", sa), ".")
	})

	t.Run("gcloud/dns+dig", func(t *testing.T) {
		g := gcloud(t)
		e.run(t, g, "dns", "managed-zones", "create", "compat-zone", "--dns-name=compat.test.", "--description=compat")
		e.run(t, g, "dns", "record-sets", "create", "www.compat.test.", "--zone=compat-zone", "--type=A", "--ttl=60", "--rrdatas=192.0.2.10")
		contains(t, "record-sets list", e.run(t, g, "dns", "record-sets", "list", "--zone=compat-zone", "--format=value(name,type)"), "www.compat.test.\tA")
		dig := onPath(t, "dig")
		host, port, _ := net.SplitHostPort(e.vars["GCPEMU_DNS"])
		for _, proto := range []string{"+notcp", "+tcp"} {
			contains(t, "dig "+proto, e.run(t, dig, "@"+host, "-p", port, "www.compat.test", "A", "+short", proto), "192.0.2.10")
		}
		contains(t, "dig NXDOMAIN", e.run(t, dig, "@"+host, "-p", port, "nope.compat.test", "A"), "status: NXDOMAIN")
	})

	t.Run("gcloud/compute", func(t *testing.T) {
		g := gcloud(t)
		for _, args := range [][]string{
			{"networks", "create", "compat-vpc", "--subnet-mode=custom"},
			{"networks", "subnets", "create", "compat-subnet", "--network=compat-vpc", "--region=us-central1", "--range=10.60.0.0/24"},
			{"routers", "create", "compat-router", "--network=compat-vpc", "--region=us-central1"},
			{"routers", "nats", "create", "compat-nat", "--router=compat-router", "--region=us-central1", "--auto-allocate-nat-external-ips", "--nat-all-subnet-ip-ranges"},
			{"firewall-rules", "create", "compat-fw", "--network=compat-vpc", "--allow=tcp:80"},
			{"addresses", "create", "compat-ip", "--global"},
			{"health-checks", "create", "http", "compat-hc", "--port=80"},
			{"backend-services", "create", "compat-bs", "--global", "--protocol=HTTP", "--health-checks=compat-hc", "--load-balancing-scheme=EXTERNAL_MANAGED", "--enable-cdn"},
			{"network-endpoint-groups", "create", "compat-neg", "--zone=us-central1-a", "--network=compat-vpc", "--subnet=compat-subnet", "--network-endpoint-type=GCE_VM_IP_PORT", "--default-port=80"},
			{"backend-services", "add-backend", "compat-bs", "--global", "--network-endpoint-group=compat-neg", "--network-endpoint-group-zone=us-central1-a", "--balancing-mode=RATE", "--max-rate-per-endpoint=100"},
			{"url-maps", "create", "compat-map", "--default-service=compat-bs"},
			{"target-http-proxies", "create", "compat-proxy", "--url-map=compat-map"},
			{"forwarding-rules", "create", "compat-fr", "--global", "--target-http-proxy=compat-proxy", "--ports=80", "--address=compat-ip", "--load-balancing-scheme=EXTERNAL_MANAGED"},
			{"url-maps", "invalidate-cdn-cache", "compat-map", "--path=/*"},
		} {
			e.run(t, g, append([]string{"compute"}, args...)...)
		}
		contains(t, "forwarding-rules list", e.run(t, g, "compute", "forwarding-rules", "list", "--format=value(name)"), "compat-fr")
		contains(t, "routers get-status", e.run(t, g, "compute", "routers", "get-status", "compat-router", "--region=us-central1", "--format=json"), "compat-nat")
		e.run(t, g, "compute", "backend-services", "get-health", "compat-bs", "--global")
	})

	t.Run("artifacts+docker+crane+ko", func(t *testing.T) {
		g := gcloud(t)
		e.run(t, g, "artifacts", "repositories", "create", "compat-repo", "--repository-format=docker", "--location=us-central1")
		reg := e.vars["GCPEMU_REGISTRY"]
		repo := reg + "/" + project + "/compat-repo"
		tok := strings.TrimSpace(e.run(t, g, "auth", "print-access-token"))

		crane := tool(t, "crane")
		login := exec.Command(crane, "auth", "login", reg, "-u", "oauth2accesstoken", "--password-stdin")
		login.Env, login.Stdin = e.env, strings.NewReader(tok)
		if out, err := login.CombinedOutput(); err != nil {
			t.Fatalf("crane auth login: %v\n%s", err, out)
		}
		if out, err := e.try(crane, "copy", "--insecure", "docker.io/library/busybox:1.36.1", repo+"/base:v1"); err != nil {
			t.Skipf("crane copy from Docker Hub (needs the internet): %v\n%s", err, out)
		}
		e.run(t, crane, "copy", "--insecure", repo+"/base:v1", repo+"/copy:v1") // cross-repo mount
		d1 := strings.TrimSpace(e.run(t, crane, "digest", "--insecure", repo+"/base:v1"))
		if d2 := strings.TrimSpace(e.run(t, crane, "digest", "--insecure", repo+"/copy:v1")); d1 != d2 {
			t.Fatalf("copied digest %s != %s", d2, d1)
		}
		contains(t, "crane ls", e.run(t, crane, "ls", "--insecure", repo+"/copy"), "v1")
		e.run(t, crane, "delete", "--insecure", repo+"/copy@"+d1)

		// ko: a multi-arch image on the emulated base.
		ko := tool(t, "ko")
		app := t.TempDir()
		_ = os.WriteFile(filepath.Join(app, "go.mod"), []byte("module example.test/koapp\n\ngo 1.22\n"), 0o644)
		_ = os.WriteFile(filepath.Join(app, "main.go"), []byte("package main\n\nfunc main() { println(\"ko\") }\n"), 0o644)
		kb := exec.Command(ko, "build", "--insecure-registry", "--bare", "--platform=linux/amd64,linux/arm64", ".")
		kb.Dir = app
		kb.Env = append(append([]string{}, e.env...), "KO_DOCKER_REPO="+repo+"/koapp", "KO_DEFAULTBASEIMAGE="+repo+"/base:v1", "GOFLAGS=-mod=mod")
		if out, err := kb.CombinedOutput(); err != nil {
			t.Fatalf("ko build: %v\n%s", err, out)
		}
		var idx struct {
			MediaType string
			Manifests []struct{ Platform struct{ Architecture string } }
		}
		if err := json.Unmarshal([]byte(e.run(t, crane, "manifest", "--insecure", repo+"/koapp:latest")), &idx); err != nil || len(idx.Manifests) != 2 {
			t.Fatalf("ko image index: %+v %v", idx, err)
		}

		// Docker: login with the gcloud token, push, pull.
		docker := onPath(t, "docker")
		dl := exec.Command(docker, "login", "-u", "oauth2accesstoken", "--password-stdin", reg)
		dl.Env, dl.Stdin = e.env, strings.NewReader(tok)
		if out, err := dl.CombinedOutput(); err != nil {
			t.Fatalf("docker login: %v\n%s", err, out)
		}
		e.run(t, docker, "pull", "-q", repo+"/base:v1")
		e.run(t, docker, "tag", repo+"/base:v1", repo+"/docker:v2")
		e.run(t, docker, "push", "-q", repo+"/docker:v2")
		e.run(t, docker, "rmi", repo+"/docker:v2", repo+"/base:v1")
		e.run(t, docker, "pull", "-q", repo+"/docker:v2")
		_, _ = e.try(docker, "rmi", repo+"/docker:v2")

		ar := "us-central1-docker.pkg.dev/" + project + "/compat-repo"
		contains(t, "artifacts docker images list", e.run(t, g, "artifacts", "docker", "images", "list", ar, "--format=value(package)"), ar+"/docker")
		contains(t, "artifacts docker tags list", e.run(t, g, "artifacts", "docker", "tags", "list", ar+"/docker"), "v2")
	})

	t.Run("gcloud/sql+psql+auth-proxy", func(t *testing.T) {
		g := gcloud(t)
		if !e.runtime {
			t.Skip("Cloud SQL needs a container runtime")
		}
		e.run(t, g, "sql", "instances", "create", "compat-pg", "--database-version=POSTGRES_17", "--tier=db-custom-1-3840",
			"--region=us-central1", "--root-password=rootpw", "--authorized-networks=0.0.0.0/0")
		ip := strings.TrimSpace(e.run(t, g, "sql", "instances", "describe", "compat-pg", "--format=value(ipAddresses[0].ipAddress)"))
		e.run(t, g, "sql", "databases", "create", "appdb", "--instance=compat-pg")
		e.run(t, g, "sql", "users", "create", "alice", "--instance=compat-pg", "--password=alicepw")
		e.run(t, g, "sql", "instances", "patch", "compat-pg", "--database-flags=max_connections=50,cloudsql.iam_authentication=on")
		e.run(t, g, "sql", "ssl", "client-certs", "create", "compat-cert", filepath.Join(t.TempDir(), "client.key"), "--instance=compat-pg")
		sa := "compat-db@" + project + ".iam.gserviceaccount.com"
		e.run(t, g, "iam", "service-accounts", "create", "compat-db")
		// Cloud SQL names IAM service account users by the email without
		// ".gserviceaccount.com".
		e.run(t, g, "sql", "users", "create", strings.TrimSuffix(sa, ".gserviceaccount.com"), "--instance=compat-pg", "--type=cloud_iam_service_account")
		contains(t, "users list", e.run(t, g, "sql", "users", "list", "--instance=compat-pg", "--format=value(name,type)"), "CLOUD_IAM_SERVICE_ACCOUNT")

		docker := onPath(t, "docker")
		psql := func(conn, password, query string) string {
			t.Helper()
			return strings.TrimSpace(e.run(t, docker, "run", "--rm", "--network=host", "-e", "PGPASSWORD="+password,
				"postgres:17.10-alpine", "psql", conn, "-tAc", query))
		}
		if got := psql("host="+ip+" user=alice dbname=appdb sslmode=prefer", "alicepw", "select current_user||':'||current_setting('max_connections')"); got != "alice:50" {
			t.Fatalf("psql on the public IP: %q", got)
		}
		hp := e.emuEnv(t)["GCPEMU_SQL_COMPAT_PG"]
		host, port, _ := net.SplitHostPort(hp)
		if got := psql("host="+host+" port="+port+" user=postgres dbname=postgres", "rootpw", "select 1"); got != "1" {
			t.Fatalf("psql on the host port %s: %q", hp, got)
		}

		// The Auth Proxy v2 against the emulated SQL Admin API: a built-in
		// user, then automatic IAM authentication as the service account.
		proxy := tool(t, "cloud-sql-proxy")
		conn := project + ":us-central1:compat-pg"
		withProxy := func(auth []string, fn func(port int)) {
			t.Helper()
			p := freePort(t)
			args := append([]string{"--sqladmin-api-endpoint", "http://" + e.vars["GCPEMU_GATEWAY"] + "/", "--port", strconv.Itoa(p)}, auth...)
			cmd := exec.Command(proxy, append(args, conn)...)
			cmd.Env = e.env
			var log strings.Builder
			cmd.Stdout, cmd.Stderr = &log, &log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			for i := 0; i < 50 && !strings.Contains(log.String(), "ready for new connections"); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if !strings.Contains(log.String(), "ready for new connections") {
				t.Fatalf("auth proxy did not start:\n%s", log.String())
			}
			fn(p)
		}
		withProxy([]string{"--token", "owner"}, func(p int) {
			if got := psql(fmt.Sprintf("host=127.0.0.1 port=%d user=postgres dbname=appdb sslmode=disable", p), "rootpw", "select current_user"); got != "postgres" {
				t.Fatalf("via the auth proxy: %q", got)
			}
		})
		// The service account's key file (its token_uri is the emulator's).
		// A static --login-token would do too, but it carries no expiry, so
		// the connector treats each certificate as expired and refreshes
		// until rate limited.
		key := filepath.Join(t.TempDir(), "db.json")
		e.run(t, g, "iam", "service-accounts", "keys", "create", key, "--iam-account", sa)
		withProxy([]string{"--credentials-file", key, "--auto-iam-authn"}, func(p int) {
			user := strings.TrimSuffix(sa, ".gserviceaccount.com")
			if got := psql(fmt.Sprintf("host=127.0.0.1 port=%d user=%s dbname=appdb sslmode=disable", p, user), "", "select current_user"); got != user {
				t.Fatalf("IAM login via the auth proxy: %q", got)
			}
		})
	})

	t.Run("gcloud/container+kubectl+helm", func(t *testing.T) {
		g := gcloud(t)
		if !e.runtime {
			t.Skip("GKE needs a container runtime")
		}
		onPath(t, "gke-gcloud-auth-plugin")
		kubectl := onPath(t, "kubectl")
		e.run(t, g, "container", "clusters", "create", "compat-cluster", "--zone=us-central1-a", "--num-nodes=1")
		e.run(t, g, "container", "clusters", "get-credentials", "compat-cluster", "--zone=us-central1-a")
		contains(t, "kubeconfig user", e.run(t, kubectl, "config", "view", "--minify", "-o", "jsonpath={.users[0].user.exec.command}"), "gke-gcloud-auth-plugin")
		contains(t, "kubectl get nodes", e.run(t, kubectl, "get", "nodes"), " Ready ")
		e.run(t, kubectl, "create", "namespace", "compat")
		contains(t, "node-pools list", e.run(t, g, "container", "node-pools", "list", "--cluster=compat-cluster", "--zone=us-central1-a", "--format=value(name)"), "default-pool")

		helm := tool(t, "helm")
		chart := t.TempDir()
		_ = os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: compat\nversion: 0.1.0\n"), 0o644)
		_ = os.MkdirAll(filepath.Join(chart, "templates"), 0o755)
		_ = os.WriteFile(filepath.Join(chart, "templates", "cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-cfg\ndata:\n  greeting: {{ .Values.greeting | quote }}\n"), 0o644)
		e.run(t, helm, "upgrade", "--install", "compat", chart, "-n", "compat", "--set", "greeting=hi", "--wait")
		if got := e.run(t, kubectl, "-n", "compat", "get", "configmap", "compat-cfg", "-o", "jsonpath={.data.greeting}"); got != "hi" {
			t.Fatalf("helm-installed configmap: %q", got)
		}
		contains(t, "helm list", e.run(t, helm, "list", "-n", "compat", "--short"), "compat")
		e.run(t, helm, "uninstall", "compat", "-n", "compat")
		e.run(t, g, "container", "clusters", "delete", "compat-cluster", "--zone=us-central1-a")
	})

	// The emulator must still be healthy after all of the above.
	resp, err := http.Get("http://" + e.vars["GCPEMU_GATEWAY"] + "/_emu/v1/ready")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("ready after the clients: %v %v", resp, err)
	}
}
