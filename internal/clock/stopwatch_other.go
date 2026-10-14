//go:build !windows

package clock

import "time"

var epoch = time.Now()

// ticks is monotonic nanoseconds since process start.
func ticks() int64 { return int64(time.Since(epoch)) }
