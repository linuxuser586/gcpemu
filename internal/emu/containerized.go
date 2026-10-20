package emu

import (
	"context"
	"errors"
	"time"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// ContainerizedWait is how long a caller deciding at start waits for the
// container runtime to say whether the Instance is a Containerized
// Instance (Containerized).
const ContainerizedWait = 3 * time.Second

// ContainerizedProbe stands in for what Containerized asks, in tests:
// Markers for runtime.InContainer, and Self for the runtime's self
// container ("" on the host's network) and whether it found it
// (runtime.Client.SelfFound), an error being no runtime.
type ContainerizedProbe struct {
	Markers func() bool
	Self    func(ctx context.Context) (self string, found bool, err error)
}

// Containerized reports whether the Instance is a Containerized Instance
// (runtime.DecideContainerized, #502). When it may be one, it asks the
// container runtime, connecting it if needed, for as long as ctx allows;
// without an answer --containerized and the marker files decide. The
// guess and any conflict are logged once (NoteContainerized).
func (e *Env) Containerized(ctx context.Context) bool {
	d, _, err := e.decideContainerized(ctx)
	if err != nil && !errors.Is(err, ErrNoRuntime) {
		// The runtime may yet answer: not the no-socket guess.
		e.Log.Info("container runtime not reachable yet; deciding whether this is a Containerized Instance without it", "err", err, "containerized", d.On)
		return d.On
	}
	e.NoteContainerized(d)
	return d.On
}

// decideContainerized is Containerized without logging: the decision,
// whether the runtime found the emulator's own container (so the decision
// is its answer), and the error asking it, if it was asked.
func (e *Env) decideContainerized(ctx context.Context) (runtime.ContainerizedDecision, bool, error) {
	f := runtime.ContainerizedFacts{Setting: e.Config.Containerized, Markers: e.markers()}
	if !runtime.MayBeContainerized(f.Setting, f.Markers) {
		return runtime.ContainerizedDecision{}, false, nil
	}
	self, found, err := e.self(ctx)
	if err == nil {
		f.Runtime, f.Found, f.Self = true, found, self
	}
	return runtime.DecideContainerized(f), f.Found, err
}

// NoteContainerized logs, once each, that d is a guess from the marker
// files or that --containerized disagrees with the runtime.
func (e *Env) NoteContainerized(d runtime.ContainerizedDecision) {
	for _, msg := range []string{d.Guess, d.Conflict} {
		if msg == "" {
			continue
		}
		if _, done := e.containerizedWarned.LoadOrStore(msg, true); !done {
			e.Log.Warn(msg)
		}
	}
}

func (e *Env) markers() bool {
	if e.ContainerizedProbe != nil {
		return e.ContainerizedProbe.Markers()
	}
	return runtime.InContainer()
}

// self asks the runtime for its self container and whether it found it,
// giving up when ctx ends even while another caller connects it.
func (e *Env) self(ctx context.Context) (string, bool, error) {
	ask := func(ctx context.Context) (string, bool, error) {
		if e.ContainerizedProbe != nil {
			return e.ContainerizedProbe.Self(ctx)
		}
		if e.Containers == nil {
			return "", false, ErrNoRuntime
		}
		rt, err := e.Containers.Runtime(ctx)
		if err != nil {
			return "", false, err
		}
		return rt.Self(), rt.SelfFound(), nil
	}
	type answer struct {
		self  string
		found bool
		err   error
	}
	ch := make(chan answer, 1)
	go func() {
		self, found, err := ask(ctx)
		ch <- answer{self, found, err}
	}()
	select {
	case a := <-ch:
		return a.self, a.found, a.err
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}
