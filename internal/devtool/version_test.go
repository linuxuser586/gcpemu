package main

import "testing"

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		have, need string
		want       bool
	}{
		{"1.56.0", "1.56.0", true},
		{"1.57.0", "1.56.0", true},
		{"1.55.9", "1.56.0", false},
		{"2.0", "1.56.0", true},
		{"1.56", "1.56.0", true}, // a missing field counts as 0
		{"1.56.0", "1.56", true},
		{"1.26.1", "1.26.2", false},
		{"1.10.0", "1.9.0", true}, // numeric, not lexical
		{"1.28rc1", "1.28", true}, // a field's non-digit suffix is dropped
		{"1.28rc1", "1.28.1", false},
		{"26.1.0", "26", true},
		{"9", "10", false},
		{"", "1", false},
		{"1", "", true},
	} {
		if got := versionAtLeast(tc.have, tc.need); got != tc.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tc.have, tc.need, got, tc.want)
		}
	}
}
