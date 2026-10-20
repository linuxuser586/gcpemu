// Package clock provides the Emulator clock (NFR-REL-002): real by default,
// or under --deterministic a fake clock that moves once per API request.
// Both can be advanced with `gcpemu time advance`.
package clock

import (
	"sync"
	"time"
)

// Clock reports the emulator's notion of time.
type Clock interface {
	Now() time.Time
}

// Stepper is a clock that moves once per API request (ADR 0020).
type Stepper interface {
	RequestStep()
}

// RequestStep moves c one request step if it is a Stepper; the real clock
// is not, so the call is a no-op there.
func RequestStep(c Clock) {
	if s, ok := c.(Stepper); ok {
		s.RequestStep()
	}
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

// RequestStep moves a fake base clock one request step.
func (o *Offset) RequestStep() { RequestStep(o.base) }

// Advance moves the clock forward by d.
func (o *Offset) Advance(d time.Duration) {
	o.mu.Lock()
	o.offset += d
	o.mu.Unlock()
}

// Fake is a deterministic clock that starts at a fixed instant and moves
// forward by Step on each RequestStep; Now never moves it, so every read
// within one request returns the same instant (ADR 0020).
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
	return f.t
}

// RequestStep moves the clock forward by Step, as an API request arriving
// at a Deterministic Instance does.
func (f *Fake) RequestStep() {
	f.mu.Lock()
	f.t = f.t.Add(f.Step)
	f.mu.Unlock()
}
