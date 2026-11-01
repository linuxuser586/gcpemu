//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/hosttest"
	"github.com/linuxuser586/gcpemu/internal/porttest"
)

// TestContainerizedSQLHostClients (#290): on a VM runtime a Containerized
// Instance publishes each public Cloud SQL instance's ports 3307 and 5432
// on a 127.0.0.x of its own and reports it as the PRIMARY address, so the
// Auth Proxy v2 and the Go connector reach the instance from the host
// (ADR 0012). Linux CI forces the VM-runtime decision, as
// TestContainerizedLBIngress's "forced" case does; Linux routes all of
// 127.0.0.0/8, so the aliases need no setup.
//
// With GCPEMU_LOOPBACK_ALIASES=true two instances get two addresses. With
// false the first takes 127.0.0.1, served on 3307 while a local
// PostgreSQL holds 127.0.0.1:5432, and a second is refused with
// FAILED_PRECONDITION naming the first. Without the VM-runtime decision
// nothing changes: the PRIMARY address is the instance's bridge IP.
//
// It builds the image from the repository's Dockerfile, whose base image
// comes from the internet, so it runs only with GCPEMU_NET_TESTS=1.
func TestContainerizedSQLHostClients(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("GCPEMU_NET_TESTS != 1: building the image pulls its base image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	if goruntime.GOOS != "linux" {
		t.Skip("forcing the VM-runtime decision makes sense only where the host routes 127.0.0.0/8")
	}
	name := fmt.Sprintf("gcpemu-e2e-sql-%d", time.Now().UnixNano())
	image := name + ":test"
	buildEmulatorImage(t, image)
	proxyBin := authProxy(t)
	socket := hosttest.SocketArgs(t)
	run := func(t *testing.T, suffix string, env ...string) (string, string) {
		args := append([]string{}, socket...)
		for _, e := range env {
			args = append(args, "-e", e)
		}
		return hosttest.RunEmulator(t, name+"-"+suffix, image, "", args), name + "-" + suffix
	}

	t.Run("aliases", func(t *testing.T) {
		gw, ctr := run(t, "aliases", "GCPEMU_VM_RUNTIME=true", "GCPEMU_LOOPBACK_ALIASES=true")
		c := &sqlClient{t: t, gw: gw}
		c.create("a")
		c.create("b")
		a, b := c.await("a"), c.await("b")
		for _, ip := range []string{a, b} {
			if p := net.ParseIP(ip); p == nil || !p.IsLoopback() || ip == "127.0.0.1" {
				t.Fatalf("PRIMARY %s, want a 127.0.0.x alias", ip)
			}
		}
		if a == b {
			t.Fatalf("both instances on %s", a)
		}
		d := c.dialer()
		for _, in := range []string{"a", "b"} {
			viaConnector(t, d, c.conn(in))
			viaAuthProxy(t, proxyBin, gw, c.conn(in))
		}
		out := hosttest.Docker(t, "exec", ctr, "/gcpemu", "doctor")
		for _, want := range []string{"loopback aliases for Cloud SQL: yes (forced by GCPEMU_LOOPBACK_ALIASES)",
			"Cloud SQL instance " + c.conn("a") + ": host clients reach it on " + a,
			"Cloud SQL instance " + c.conn("b") + ": host clients reach it on " + b} {
			if !strings.Contains(out, want) {
				t.Errorf("gcpemu doctor lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("127.0.0.1 only", func(t *testing.T) {
		if l, err := net.Listen("tcp", "127.0.0.1:3307"); err != nil {
			t.Skipf("127.0.0.1:3307 is taken: %v", err)
		} else {
			l.Close()
		}
		// A local PostgreSQL, unless one holds the port already.
		if l, err := net.Listen("tcp", "127.0.0.1:5432"); err == nil {
			t.Cleanup(func() { l.Close() })
		}
		gw, _ := run(t, "single", "GCPEMU_VM_RUNTIME=true", "GCPEMU_LOOPBACK_ALIASES=false")
		c := &sqlClient{t: t, gw: gw}
		c.create("first")
		if ip := c.await("first"); ip != "127.0.0.1" {
			t.Fatalf("PRIMARY %s, want 127.0.0.1", ip)
		}
		viaAuthProxy(t, proxyBin, gw, c.conn("first"))
		code, body := c.call(http.MethodPost, "", instanceBody("second"))
		if code != http.StatusBadRequest || !bytes.Contains(body, []byte("FAILED_PRECONDITION")) || !bytes.Contains(body, []byte(c.conn("first"))) {
			t.Fatalf("second public instance: %d %s, want FAILED_PRECONDITION naming %s", code, body, c.conn("first"))
		}
	})

	t.Run("not a VM runtime", func(t *testing.T) {
		gw, _ := run(t, "off", "GCPEMU_VM_RUNTIME=false", "GCPEMU_LOOPBACK_ALIASES=true")
		c := &sqlClient{t: t, gw: gw}
		c.create("plain")
		if ip := net.ParseIP(c.await("plain")); ip == nil || ip.IsLoopback() {
			t.Fatalf("PRIMARY %s, want the instance's bridge IP", ip)
		}
		ports := hosttest.Docker(t, "port", c.container("plain"))
		if strings.Contains(ports, "3307") || strings.Count(strings.TrimSpace(ports), "\n") != 0 {
			t.Errorf("instance container publishes %q, want only its GCPEMU_SQL_* port", ports)
		}
		viaConnector(t, c.dialer(), c.conn("plain"))
	})
}

const sqlProject, sqlRegion = "e2e-sql", "us-central1"

// sqlClient creates Cloud SQL instances through the emulator's SQL Admin
// API at gw.
type sqlClient struct {
	t  *testing.T
	gw string
}

func instanceBody(name string) map[string]any {
	return map[string]any{"name": name, "databaseVersion": "POSTGRES_18", "region": sqlRegion, "rootPassword": "rootpw",
		"settings": map[string]any{"ipConfiguration": map[string]any{"ipv4Enabled": true}}}
}

func (c *sqlClient) call(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+c.gw+"/sql/v1beta4/projects/"+sqlProject+"/instances"+path, r)
	req.Header.Set("Authorization", "Bearer owner")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c *sqlClient) create(name string) {
	c.t.Helper()
	if code, body := c.call(http.MethodPost, "", instanceBody(name)); code != http.StatusOK {
		c.t.Fatalf("creating %s: %d %s", name, code, body)
	}
}

// await waits for instance name to be RUNNABLE and returns its PRIMARY
// address.
func (c *sqlClient) await(name string) string {
	c.t.Helper()
	var in struct {
		State       string
		IpAddresses []struct{ Type, IpAddress string }
	}
	hosttest.Eventually(c.t, 5*time.Minute, "instance "+name+" RUNNABLE", func() bool {
		code, body := c.call(http.MethodGet, "/"+name, nil)
		return code == http.StatusOK && json.Unmarshal(body, &in) == nil && (in.State == "RUNNABLE" || in.State == "FAILED")
	}, nil)
	if in.State != "RUNNABLE" {
		c.t.Fatalf("instance %s is %s", name, in.State)
	}
	for _, a := range in.IpAddresses {
		if a.Type == "PRIMARY" {
			return a.IpAddress
		}
	}
	c.t.Fatalf("instance %s has no PRIMARY address: %+v", name, in.IpAddresses)
	return ""
}

func (c *sqlClient) conn(name string) string { return sqlProject + ":" + sqlRegion + ":" + name }

// container is the name of instance name's container.
func (c *sqlClient) container(name string) string {
	c.t.Helper()
	out := hosttest.Docker(c.t, "ps", "-q", "--filter", "label=gcpemu.service=sql", "--filter", "label=gcpemu.resource=projects/"+sqlProject+"/instances/"+name)
	if out == "" || strings.Contains(out, "\n") {
		c.t.Fatalf("containers of instance %s: %q", name, out)
	}
	return out
}

// dialer is a Go connector against the emulator's SQL Admin API.
func (c *sqlClient) dialer() *cloudsqlconn.Dialer {
	c.t.Helper()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "owner", Expiry: time.Now().Add(time.Hour)})
	d, err := cloudsqlconn.NewDialer(context.Background(), cloudsqlconn.WithAdminAPIEndpoint("http://"+c.gw+"/"), cloudsqlconn.WithTokenSource(ts))
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { d.Close() })
	return d
}

// viaConnector runs a query on instance icn through the Go connector.
func viaConnector(t *testing.T, d *cloudsqlconn.Dialer, icn string) {
	t.Helper()
	cfg, err := pgx.ParseConfig("user=postgres dbname=postgres sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = "rootpw"
	cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx, icn) }
	query(t, cfg, "the Go connector to "+icn)
}

// viaAuthProxy runs a query on instance icn through the Auth Proxy v2.
func viaAuthProxy(t *testing.T, bin, gw, icn string) {
	t.Helper()
	// The proxy is another process and binds --port itself, so the test
	// cannot hold the port for it: another process's explicit bind can
	// still take it first (#743).
	port := porttest.Free(t)
	cmd := exec.Command(bin, "--sqladmin-api-endpoint", "http://"+gw+"/", "--token", "owner", "--port", strconv.Itoa(port), icn)
	// A static --token is in Google's universe, so the proxy refuses it
	// under another universe domain.
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOOGLE_CLOUD_UNIVERSE_DOMAIN=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	var log strings.Builder
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("auth proxy log:\n%s", log.String())
		}
	}()
	hosttest.Eventually(t, 10*time.Second, "the auth proxy to start", func() bool {
		return strings.Contains(log.String(), "ready for new connections")
	}, log.String)
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d user=postgres dbname=postgres sslmode=disable", port))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = "rootpw"
	query(t, cfg, "the Auth Proxy to "+icn)
}

func query(t *testing.T, cfg *pgx.ConnConfig, via string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting through %s: %v", via, err)
	}
	defer conn.Close(ctx)
	var who string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&who); err != nil || who != "postgres" {
		t.Fatalf("querying through %s: %q %v", via, who, err)
	}
}

// authProxySHA256 pins the Cloud SQL Auth Proxy v2 by platform.
var authProxySHA256 = map[string]string{
	"linux/amd64": "39ed2abcaa073c3b4714585e36a904e2b1d92218c5e07c18224fb73ef4bedde5",
	"linux/arm64": "8ba6ebca26cb2249b8d623fd3736d72e4b57e52bb19738515e936a01201833e5",
}

const authProxyVersion = "v2.27.0"

// authProxy downloads the pinned Auth Proxy for the host into the cache
// directory (once), verifying its checksum.
func authProxy(t *testing.T) string {
	t.Helper()
	plat := goruntime.GOOS + "/" + goruntime.GOARCH
	want, ok := authProxySHA256[plat]
	if !ok {
		t.Skipf("no pinned cloud-sql-proxy for %s", plat)
	}
	bin := filepath.Join(cacheDir(t), "cloud-sql-proxy-"+authProxyVersion, "cloud-sql-proxy")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	b := fetch(t, "https://storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/"+authProxyVersion+
		"/cloud-sql-proxy."+strings.ReplaceAll(plat, "/", "."))
	if got := sha256.Sum256(b); hex.EncodeToString(got[:]) != want {
		t.Fatalf("cloud-sql-proxy: checksum mismatch (got %x, want %s)", got, want)
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
