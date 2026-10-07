package emu

import "testing"

// TestDeterministicHex: counter-derived hex IDs of any length (compute
// operation names use 7 bytes) are reproducible and distinct.
func TestDeterministicHex(t *testing.T) {
	for _, n := range []int{4, 7, 8, 16} {
		a, b := NewIDs(true), NewIDs(true)
		x, y := a.Hex(n), b.Hex(n)
		if len(x) != 2*n || x != y {
			t.Fatalf("Hex(%d) = %q, %q", n, x, y)
		}
		if z := a.Hex(n); z == x {
			t.Fatalf("Hex(%d) repeated %q", n, z)
		}
	}
	if got := NewIDs(true).Hex(7); got != "00000000000001" {
		t.Fatalf("Hex(7) = %q", got)
	}
}
