package hostmode_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	mdns "github.com/miekg/dns"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/hostmode"
)

func TestHostsBlockNames(t *testing.T) {
	got := hostmode.HostsBlockNames([]string{"accounts.google.com", "storage.googleapis.com", "pubsub.googleapis.com"})
	for _, want := range []string{"storage.googleapis.com", "pubsub.googleapis.com", "us-docker.pkg.dev", "europe-west1-docker.pkg.dev"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if slices.Contains(got, "accounts.google.com") {
		t.Error("accounts.google.com must not be redirected on the host")
	}
}

// TestHostMode starts an instance with --host-mode and reaches it by real
// hostnames from the host: DNS through the frontend container, curl with
// --resolve and the Go storage client with a resolver pointed at it
// (FR-CORE-043).
func TestHostMode(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"gcs", "pubsub", "ar", "dns"}, func(c *config.Config) { c.HostMode = true })
	var st hostmode.Status
	deadline := time.Now().Add(90 * time.Second)
	for st = inst.HostMode(); st.State == hostmode.StateStarting; st = inst.HostMode() {
		if time.Now().After(deadline) {
			t.Fatal("host mode not ready after 90s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if st.State != hostmode.StateReady || st.IP == "" {
		t.Fatalf("host mode = %+v", st)
	}
	if env := inst.EnvVars(); env["GCPEMU_HOST_MODE_IP"] != st.IP || env["GCPEMU_CA_FILE"] != inst.Env.CA.Path() {
		t.Errorf("env = %v", env)
	}
	dnsAddr := net.JoinHostPort(st.IP, "53")

	t.Run("DNS", func(t *testing.T) {
		for _, name := range []string{"storage.googleapis.com.", "pubsub.googleapis.com.", "us-central1-docker.pkg.dev."} {
			m := new(mdns.Msg)
			m.SetQuestion(name, mdns.TypeA)
			r, _, err := new(mdns.Client).Exchange(m, dnsAddr)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != st.IP {
				t.Errorf("%s → %v, want %s", name, r.Answer, st.IP)
			}
		}
	})

	// A resolver that asks the host-mode DNS, as systemd-resolved
	// per-domain routing would.
	res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, dnsAddr)
	}}
	tr := &http.Transport{
		DialContext:       (&net.Dialer{Resolver: res, Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:   &tls.Config{RootCAs: inst.Env.CA.Pool()},
		ForceAttemptHTTP2: true,
	}
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	ctx := context.Background()

	t.Run("GoStorageClient", func(t *testing.T) {
		sc, err := storage.NewClient(ctx, option.WithHTTPClient(hc))
		if err != nil {
			t.Fatal(err)
		}
		defer sc.Close()
		b := sc.Bucket("hostmode-bucket")
		if err := b.Create(ctx, "hostmode", nil); err != nil {
			t.Fatalf("create bucket: %v", err)
		}
		w := b.Object("o.txt").NewWriter(ctx)
		_, _ = io.WriteString(w, "real hostname")
		if err := w.Close(); err != nil {
			t.Fatalf("upload: %v", err)
		}
		attrs, err := b.Object("o.txt").Attrs(ctx)
		if err != nil || attrs.Size != int64(len("real hostname")) {
			t.Fatalf("attrs = %+v, %v", attrs, err)
		}
	})

	t.Run("Curl", func(t *testing.T) {
		if _, err := exec.LookPath("curl"); err != nil {
			t.Skip("curl not installed")
		}
		out, err := exec.Command("curl", "-sS", "--http2", "--cacert", inst.Env.CA.Path(),
			"--resolve", "storage.googleapis.com:443:"+st.IP,
			"-o", "/dev/null", "-w", "%{http_code} %{http_version}",
			"https://storage.googleapis.com/storage/v1/b?project=hostmode").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "200 2" {
			t.Errorf("curl = %q, %v", out, err)
		}
	})

	t.Run("PlainHTTP", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://"+st.IP+"/storage/v1/b?project=hostmode", nil)
		req.Host = "storage.googleapis.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET over :80 = %d", resp.StatusCode)
		}
	})
}
