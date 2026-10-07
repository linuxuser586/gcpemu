package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/hostmode"
	"github.com/linuxuser586/gcpemu/services"
)

func TestWriteHostsBlock(t *testing.T) {
	var b bytes.Buffer
	_ = writeHostsBlock(&b, "dev", hostmode.Status{IP: "172.18.0.5", Hosts: []string{"pubsub.googleapis.com", "us-docker.pkg.dev"}})
	want := "# BEGIN gcpemu dev (host mode)\n172.18.0.5\tpubsub.googleapis.com\n172.18.0.5\tus-docker.pkg.dev\n# END gcpemu dev\n"
	if b.String() != want {
		t.Errorf("block =\n%s", b.String())
	}
}

// TestEnvTrust checks `gcpemu env` (FR-CORE-004): GCPEMU_CA_FILE always,
// SSL_CERT_FILE with --trust pointing at a bundle of system roots plus the
// CA; and `gcpemu hosts` without host mode.
func TestEnvTrust(t *testing.T) {
	inst := emutest.Start(t, []string{"gcs"})
	run := func(args ...string) (string, string, error) {
		cmd := New(services.Factories())
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(append(args, "--data-dir", inst.Config.DataDir, "--config", os.DevNull))
		err := cmd.Execute()
		return out.String(), errOut.String(), err
	}
	out, _, err := run("env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "export GCPEMU_CA_FILE='"+inst.Env.CA.Path()+"'") || strings.Contains(out, "SSL_CERT_FILE") {
		t.Errorf("env =\n%s", out)
	}
	if !strings.Contains(out, "export STORAGE_EMULATOR_HOST=") {
		t.Errorf("existing variables missing:\n%s", out)
	}
	out, _, err = run("env", "--trust")
	if err != nil {
		t.Fatal(err)
	}
	var bundle string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "export SSL_CERT_FILE="); ok {
			bundle = strings.Trim(v, "'")
		}
	}
	b, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("SSL_CERT_FILE %q: %v\n%s", bundle, err, out)
	}
	if !bytes.Contains(b, bytes.TrimSpace(inst.Env.CA.PEM())) || bytes.Count(b, []byte("BEGIN CERTIFICATE")) < 2 {
		t.Errorf("bundle has %d certificates and CA=%v", bytes.Count(b, []byte("BEGIN CERTIFICATE")), bytes.Contains(b, inst.Env.CA.PEM()))
	}
	if _, _, err := run("hosts"); err == nil || !strings.Contains(err.Error(), "--host-mode") {
		t.Errorf("hosts without host mode: %v", err)
	}
}
