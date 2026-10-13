package runtime

import (
	"errors"
	"testing"
)

func TestUnusableOn(t *testing.T) {
	for goos, want := range map[string]error{
		"linux":   nil,
		"darwin":  nil,
		"windows": ErrNeedsWSL2,
	} {
		if got := UnusableOn(goos); !errors.Is(got, want) || (want == nil) != (got == nil) {
			t.Errorf("UnusableOn(%q) = %v, want %v", goos, got, want)
		}
	}
}
