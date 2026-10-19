package inflight

import (
	"context"
	"errors"
	"sync"
)

// ErrReset is the cancel cause of a Loops generation that Reset ended.
var ErrReset = errors.New("inflight: background work halted by an Instance Reset")

// Halted reports whether ctx was cancelled by a Loops Reset. Work that
// finds it so drops the side effects of state it read before the Reset.
func Halted(ctx context.Context) bool { return errors.Is(context.Cause(ctx), ErrReset) }

// Loops runs a service's background loops in generations (#307): Reset
// cancels the current generation and waits for its loops, so no iteration
// from before an Instance Reset outlives it, and Resume starts them again
// once the Store is reset too (emu.ResetFinisher). The zero value is ready
// to use.
type Loops struct {
	mu      sync.Mutex
	fns     []func(context.Context)
	cancel  context.CancelCauseFunc
	done    chan struct{} // closed once the generation's loops returned
	running bool          // between Start and Stop
	paused  bool          // between Reset and Resume
}

// Start runs each of fns in a goroutine until Stop, unless the loops are
// already running.
func (l *Loops) Start(fns ...func(ctx context.Context)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return
	}
	l.fns, l.running, l.paused = fns, true, false
	l.startLocked()
}

// startLocked starts a generation of the loops. Callers hold mu.
func (l *Loops) startLocked() {
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(l.fns))
	for _, f := range l.fns {
		go func() {
			defer wg.Done()
			f(ctx)
		}()
	}
	go func() { wg.Wait(); close(done) }()
	l.cancel, l.done = cancel, done
}

// Stop cancels the loops and waits for them, or for ctx to end.
func (l *Loops) Stop(ctx context.Context) error {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	l.running = false
	l.cancel(context.Canceled)
	done := l.done
	l.mu.Unlock()
	return wait(ctx, done)
}

// Reset cancels the current generation with ErrReset and waits for its
// loops, or for ctx to end, which it reports. The loops stay stopped until
// Resume.
func (l *Loops) Reset(ctx context.Context) error {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	l.paused = true
	l.cancel(ErrReset)
	done := l.done
	l.mu.Unlock()
	return wait(ctx, done)
}

// Resume starts the loops again after Reset, unless Stop ran meanwhile.
// Loops of a Reset that ended before they returned may still be finishing.
func (l *Loops) Resume() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running && l.paused {
		l.paused = false
		l.startLocked()
	}
}

func wait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
