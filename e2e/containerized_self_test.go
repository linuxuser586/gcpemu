//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

const selfProject, selfRegion, selfRange = "e2e-self", "us-central1", "10.213.77.0/24"

// TestContainerizedSelfAddress (#811): once a subnetwork is realised as a
// VPC container network, a Containerized Instance's container holds its
// Self address there, and an internal Address that asks for it is refused
// with ipInUseByAnotherResource, as the runtime would refuse the
// container that later took it.
//
// It builds the image from the repository's Dockerfile, whose base image
// comes from the internet, so it runs only with GCPEMU_NET_TESTS=1.
func TestContainerizedSelfAddress(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("GCPEMU_NET_TESTS != 1: building the image pulls its base image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	name := fmt.Sprintf("gcpemu-e2e-self-%d", time.Now().UnixNano())
	image := name + ":test"
	buildEmulatorImage(t, image)
	gw := hosttest.RunEmulator(t, name, image, "", hosttest.SocketArgs(t))

	compute := func(method, path string, body any) (int, []byte) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, "http://"+gw+"/compute/v1/projects/"+selfProject+"/"+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"global/networks", map[string]any{"name": "self", "autoCreateSubnetworks": false}},
		{"regions/" + selfRegion + "/subnetworks", map[string]any{"name": "self", "network": "global/networks/self", "ipCidrRange": selfRange}},
	} {
		if code, out := compute(http.MethodPost, c.path, c.body); code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", c.path, code, out)
		}
	}

	// Asking for the subnetwork's Host address realises it, whatever the
	// answer. The admin API needs the token beyond loopback.
	sp := "projects/" + selfProject + "/regions/" + selfRegion + "/subnetworks/self"
	req, _ := http.NewRequest(http.MethodGet, "http://"+gw+"/_emu/v1/host/address?subnetwork="+url.QueryEscape(sp), nil)
	req.Header.Set(gateway.AdminTokenHeader, hosttest.Docker(t, "exec", name, "/gcpemu", "admin", "token"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	_, rng, _ := net.ParseCIDR(selfRange)
	var self string
	var nets map[string]struct{ IPAddress string }
	if err := json.Unmarshal([]byte(hosttest.Docker(t, "inspect", "-f", "{{json .NetworkSettings.Networks}}", name)), &nets); err != nil {
		t.Fatal(err)
	}
	for n, ep := range nets {
		if strings.Contains(n, "-vpc-self-self-") {
			self = ep.IPAddress
		}
	}
	if ip := net.ParseIP(self); ip == nil {
		t.Fatalf("the emulator's container is on no VPC container network of subnetwork self: %v", nets)
	} else if !rng.Contains(ip) {
		t.Skipf("%s overlaps a network on this host: the Self address %s is in the Fallback range", selfRange, self)
	}

	code, out := compute(http.MethodPost, "regions/"+selfRegion+"/addresses",
		map[string]any{"name": "self", "addressType": "INTERNAL", "subnetwork": "regions/" + selfRegion + "/subnetworks/self", "address": self})
	if code != http.StatusBadRequest || !bytes.Contains(out, []byte("ipInUseByAnotherResource")) {
		t.Fatalf("reserving the Self address %s: %d %s, want 400 ipInUseByAnotherResource", self, code, out)
	}
	if code, out := compute(http.MethodPost, "regions/"+selfRegion+"/addresses",
		map[string]any{"name": "auto", "addressType": "INTERNAL", "subnetwork": "regions/" + selfRegion + "/subnetworks/self"}); code != http.StatusOK {
		t.Fatalf("reserving an internal address: %d %s", code, out)
	}
}
