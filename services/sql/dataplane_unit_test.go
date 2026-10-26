package sql

import (
	"errors"
	"testing"
)

// The runtime says "Address already in use" both for a taken container
// address and for a taken host port. Only the first moves a Cloud SQL
// instance's Public IP; a taken host port picks another port and keeps the address
// (#673).
func TestPublicIPTaken(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{"failed to set up container networking: Address already in use", true},
		{"driver failed programming external connectivity on endpoint gcpemu-x-sql: Error starting userland proxy: listen tcp4 127.0.0.1:41234: bind: address already in use", false},
		{"rootlessport listen tcp 127.0.0.1:41234: bind: address already in use", false},
		// Another port on the same address is not the instance's.
		{"listen tcp4 127.0.0.1:412345: bind: address already in use", true},
		{"Bind for 127.0.0.1:41234 failed: port is already allocated", false},
		{"some other failure", false},
	} {
		if got := publicIPTaken(errors.New(c.msg), "127.0.0.1", 41234); got != c.want {
			t.Errorf("publicIPTaken(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
	if publicIPTaken(nil, "127.0.0.1", 41234) {
		t.Error("publicIPTaken(nil) = true")
	}
}
