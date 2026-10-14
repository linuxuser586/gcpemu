package clock

import "time"

// Stopwatch times short intervals such as a loopback round trip. Go's
// monotonic clock on Windows advances only every ~0.5ms, so a fast hop
// timed with time.Since reads as zero there; Stopwatch reads a
// high-resolution counter instead.
type Stopwatch struct{ t0 int64 }

// StartStopwatch starts a Stopwatch now.
func StartStopwatch() Stopwatch { return Stopwatch{ticks()} }

// Elapsed is the time since the Stopwatch started.
func (s Stopwatch) Elapsed() time.Duration { return time.Duration(ticks() - s.t0) }
