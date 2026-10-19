package emu

import (
	"regexp"
	"testing"
)

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

// TestUUID: UUID-formatted IDs are 8-4-4-4-12 lower-case hex, counter-derived
// in deterministic mode and random otherwise.
func TestUUID(t *testing.T) {
	g := NewIDs(true)
	if got := g.UUID(); got != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("UUID() = %q", got)
	}
	if got := g.UUID(); got != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("second UUID() = %q", got)
	}
	r := NewIDs(false)
	a, b := r.UUID(), r.UUID()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(a) || a == b {
		t.Fatalf("random UUID() = %q, %q", a, b)
	}
}
