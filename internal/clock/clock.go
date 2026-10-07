// Package clock provides the emulator's clock: real by default, a fake
// advanceable clock under --deterministic or `gcpemu time advance` (NFR-REL-002).
package clock

import (
	"sync"
	"time"
)

// Clock reports the emulator's notion of time.
type Clock interface {
	Now() time.Time
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Offset is a real clock that can be advanced for lifecycle/TTL tests.
type Offset struct {
	mu     sync.RWMutex
	offset time.Duration
	base   Clock
}

// NewOffset wraps base with an advanceable offset.
func NewOffset(base Clock) *Offset { return &Offset{base: base} }

func (o *Offset) Now() time.Time {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.base.Now().Add(o.offset)
}

// Advance moves the clock forward by d.
func (o *Offset) Advance(d time.Duration) {
	o.mu.Lock()
	o.offset += d
	o.mu.Unlock()
}

// Fake is a deterministic clock that starts at a fixed instant and ticks
// forward by Step on every call to Now.
type Fake struct {
	mu   sync.Mutex
	t    time.Time
	Step time.Duration
}

// NewFake returns a fake clock starting at 2026-01-01T00:00:00Z.
func NewFake() *Fake {
	return &Fake{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Step: time.Millisecond}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.t
	f.t = f.t.Add(f.Step)
	return t
}
