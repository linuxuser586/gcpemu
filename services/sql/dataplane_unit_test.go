package sql

import (
	"errors"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// The runtime says "Address already in use" both for a taken container
// address and for a taken host port. The second names the binding, and
// only picks another host port (#673).
func TestHostPortTaken(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{"failed to set up container networking: Address already in use", false},
		{"driver failed programming external connectivity on endpoint gcpemu-x-sql: Error starting userland proxy: listen tcp4 127.0.0.1:41234: bind: address already in use", true},
		{"rootlessport listen tcp 127.0.0.1:41234: bind: address already in use", true},
		// Another port on the same address is not the instance's.
		{"listen tcp4 127.0.0.1:412345: bind: address already in use", false},
		{"some other failure", false},
	} {
		if got := hostPortTaken(errors.New(c.msg), "127.0.0.1", 41234); got != c.want {
			t.Errorf("hostPortTaken(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

// After a failed start, a Cloud SQL instance's Public IP moves only when
// something outside the emulator holds it or it left the External
// network's range; a holder of the Instance's own is an error, and with no
// holder the next start asks for it again (ADR 0035).
func TestNextPublicIP(t *testing.T) {
	const ip, subnet, inst = "172.21.0.3", "172.21.0.0/16", "i1"
	ours := runtime.Container{ID: "n", Name: "gke-node", Labels: map[string]string{runtime.LabelInstance: inst}}
	other := runtime.Container{ID: "o", Name: "gcpemu-i2-x", Labels: map[string]string{runtime.LabelInstance: "i2"}}
	stranger := runtime.Container{ID: "s", Name: "by-hand"}
	for _, c := range []struct {
		name    string
		subnet  string
		holder  runtime.Container
		held    bool
		want    string
		wantErr bool
	}{
		{name: "no holder", subnet: subnet, want: ip},
		{name: "outside the emulator", subnet: subnet, holder: stranger, held: true, want: ""},
		{name: "another Instance", subnet: subnet, holder: other, held: true, want: ""},
		{name: "another Service of the Instance", subnet: subnet, holder: ours, held: true, wantErr: true},
		{name: "outside the range", subnet: "172.22.0.0/16", want: ""},
		{name: "outside the range and held", subnet: "172.22.0.0/16", holder: ours, held: true, want: ""},
	} {
		got, err := nextPublicIP(ip, c.subnet, inst, c.holder, c.held)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: nextPublicIP = %q, %v; want %q, error %v", c.name, got, err, c.want, c.wantErr)
		}
	}
}
