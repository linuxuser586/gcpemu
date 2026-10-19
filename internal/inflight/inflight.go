// Package inflight tracks a service's background work across Instance
// Resets. Work started before a Reset must not write into the store the
// instance resets afterwards. Tracker counts deferred work (#301): Reset
// starts a new generation, so work that noted the old one knows to drop
// itself, and waits for work already running. Loops runs background loops
// that Reset halts, waits for and later resumes (#307).
package inflight

import (
	"context"
	"sync"
)

// Tracker counts running work and the generations Reset starts. The zero
// value is ready to use.
type Tracker struct {
	mu   sync.Mutex
	gen  uint64
	n    int
	idle chan struct{} // closed when n drops to zero; nil while it is zero
}

// Gen returns the current generation.
func (t *Tracker) Gen() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// Next starts a new generation. Callers hold the lock that guards their
// pending work, so that work either started before Next (and is waited for)
// or sees the new generation.
func (t *Tracker) Next() {
	t.mu.Lock()
	t.gen++
	t.mu.Unlock()
}

// Enter counts work that starts running.
func (t *Tracker) Enter() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n == 0 {
		t.idle = make(chan struct{})
	}
	t.n++
}

// Exit uncounts work counted by Enter.
func (t *Tracker) Exit() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n--
	if t.n == 0 {
		close(t.idle)
		t.idle = nil
	}
}

// Wait waits until no work is running, or returns ctx's error when it ends
// first.
func (t *Tracker) Wait(ctx context.Context) error {
	t.mu.Lock()
	idle := t.idle
	t.mu.Unlock()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
