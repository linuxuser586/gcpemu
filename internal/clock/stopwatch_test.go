package clock

import (
	"testing"
	"time"
)

// A Stopwatch resolves intervals well under Windows' ~0.5ms monotonic
// clock tick (#206).
func TestStopwatchResolvesMicroseconds(t *testing.T) {
	for range 100 {
		sw := StartStopwatch()
		x := 0
		for i := range 20000 { // a few µs of work
			x += i
		}
		_ = x
		d := sw.Elapsed()
		if d <= 0 || d > time.Second {
			t.Fatalf("Elapsed = %v, want a small positive duration", d)
		}
	}
}

func TestStopwatchMonotonic(t *testing.T) {
	sw := StartStopwatch()
	prev := sw.Elapsed()
	for range 10000 {
		d := sw.Elapsed()
		if d < prev {
			t.Fatalf("Elapsed went backwards: %v after %v", d, prev)
		}
		prev = d
	}
}
