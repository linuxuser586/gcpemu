//go:build prerelease

package prerelease

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/instance"
)

// The parsers of the resolver-routes check run on any host: they read
// output, not the resolver.

func TestAddresses(t *testing.T) {
	for _, tc := range []struct{ goos, out string }{
		{"darwin", "name: www.prerelease.test\nip_address: 192.0.2.53\n\n"},
		{"linux", "www.prerelease.test: 192.0.2.53                   -- link: gcpemu-dns0\n\n-- Information acquired via protocol DNS in 1.2ms.\n-- Data is authenticated: no; Data was acquired via local or encrypted transport: no\n-- Data from: network\n"},
		{"windows", "192.0.2.53\r\n"},
	} {
		if got := addresses(tc.out); !slices.Equal(got, []string{"192.0.2.53"}) {
			t.Errorf("%s: addresses = %v, want [192.0.2.53]", tc.goos, got)
		}
	}
	if got := addresses("-- Information acquired via protocol DNS in 1.2ms.\n"); len(got) != 0 {
		t.Errorf("no answer: addresses = %v", got)
	}
}

func TestRoutesOwner(t *testing.T) {
	refused := "Error: instance \"dev\" has the Resolver route for svc.cluster.local; run `gcpemu --instance dev dns uninstall --suffix svc.cluster.local` first\n"
	if got := routesOwner(refused); got != "dev" {
		t.Errorf("routesOwner = %q, want dev", got)
	}
	if got := routesOwner("Error: Windows' NRPT can't name a port\n"); got != "" {
		t.Errorf("another error: routesOwner = %q, want none", got)
	}
}

// TestPort53InUse is #921: step 6's skip names the data dir and Instance
// ID of a gcpemu Instance whose dns endpoint holds port 53, and otherwise
// says what it always has.
func TestPort53InUse(t *testing.T) {
	root := t.TempDir()
	exited := exec.Command(os.Args[0], "-test.run=^$")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	dataDir := func(name, dns string, pid int) string {
		dir := filepath.Join(root, name)
		eps, _ := json.Marshal(map[string]string{"gateway": "127.0.0.1:1", "dns": dns})
		for file, b := range map[string][]byte{
			instance.EndpointsFile: eps,
			instance.PIDFile:       []byte(strconv.Itoa(pid)),
			instance.IDFile:        []byte("id-" + name),
		} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, file), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	other := dataDir("other", "127.0.0.1:5353", os.Getpid())
	dead := dataDir("dead", "127.0.0.1:53", exited.Process.Pid)
	holder := dataDir("holder", "127.0.0.1:53", os.Getpid())
	inUse := errors.New("listen tcp 127.0.0.1:53: bind: address already in use")

	msg := port53InUse(inUse, []string{other, dead, holder, filepath.Join(root, "missing")})
	for _, want := range []string{holder, "id-holder", inUse.Error()} {
		if !strings.Contains(msg, want) {
			t.Errorf("skip message %q does not name %q", msg, want)
		}
	}
	const plain = "port 53 is in use on the host (ICS, Hyper-V or WSL's DNS proxy may hold it), and NRPT sends queries there: "
	if msg := port53InUse(inUse, []string{other, dead}); msg != plain+inUse.Error() {
		t.Errorf("skip message without a gcpemu holder = %q; want %q", msg, plain+inUse.Error())
	}
}
