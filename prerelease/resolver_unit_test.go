//go:build prerelease

package prerelease

import (
	"slices"
	"testing"
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
