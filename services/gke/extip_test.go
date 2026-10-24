package gke

import (
	"errors"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/netplane"
)

var testExt = netplane.Net{Name: "gcpemu-x-ext", Subnet: "172.25.0.0/16", Gateway: "172.25.0.1"}

var errInUse = errors.New("Error response from daemon: failed to set up container networking: Address already in use")

func TestExternalIPKeepsRecorded(t *testing.T) {
	var tried []string
	ip, err := withExternalIP(testExt, "172.25.0.7", func(ip string) error {
		tried = append(tried, ip)
		return nil
	})
	if err != nil || ip != "172.25.0.7" || len(tried) != 1 {
		t.Fatalf("withExternalIP = %q, %v after %v; want 172.25.0.7 at once", ip, err, tried)
	}
}

func TestExternalIPOutsideSubnet(t *testing.T) {
	ip, err := withExternalIP(testExt, "172.31.0.2", func(string) error { return nil })
	if err != nil || !netplane.Contains(testExt.Subnet, ip) {
		t.Fatalf("withExternalIP = %q, %v; want an address in %s", ip, err, testExt.Subnet)
	}
}

func TestExternalIPTakenMovesOn(t *testing.T) {
	var tried []string
	ip, err := withExternalIP(testExt, "172.25.0.7", func(ip string) error {
		tried = append(tried, ip)
		if len(tried) < 3 {
			return errInUse
		}
		return nil
	})
	if err != nil || len(tried) != 3 || ip != tried[2] {
		t.Fatalf("withExternalIP = %q, %v after %v; want the third address", ip, err, tried)
	}
	if tried[0] != "172.25.0.7" || tried[1] == tried[0] || tried[2] == tried[1] || tried[2] == tried[0] {
		t.Errorf("tried %v; want the recorded address, then two new ones", tried)
	}
}

func TestExternalIPOtherErrorStops(t *testing.T) {
	boom := errors.New("image not found")
	n := 0
	_, err := withExternalIP(testExt, "", func(string) error { n++; return boom })
	if !errors.Is(err, boom) || n != 1 {
		t.Fatalf("withExternalIP = %v after %d attempts; want %v at once", err, n, boom)
	}
}

func TestExternalIPExhausted(t *testing.T) {
	n := 0
	_, err := withExternalIP(testExt, "", func(string) error { n++; return errInUse })
	if err == nil || !strings.Contains(err.Error(), testExt.Name) || n != externalIPAttempts {
		t.Fatalf("withExternalIP = %v after %d attempts; want an error naming %s after %d", err, n, testExt.Name, externalIPAttempts)
	}
}
