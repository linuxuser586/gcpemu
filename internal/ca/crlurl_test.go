package ca_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestCRLURL: the CRL URL names the API gateway's listener address, with
// an unspecified --bind made 127.0.0.1, as the Instance builds it.
func TestCRLURL(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:4443":     "http://127.0.0.1:4443/ca.crl",
		"0.0.0.0:4443":       "http://127.0.0.1:4443/ca.crl",
		"[::]:4443":          "http://127.0.0.1:4443/ca.crl",
		"192.168.1.20:50123": "http://192.168.1.20:50123/ca.crl",
		"[::1]:4443":         "http://[::1]:4443/ca.crl",
	} {
		if got := ca.CRLURL(emu.Dialable(addr)); got != want {
			t.Errorf("CRLURL(Dialable(%q)) = %q, want %q", addr, got, want)
		}
	}
}
