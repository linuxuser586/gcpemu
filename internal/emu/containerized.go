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
// container ("" on the host's network), an error being no answer.
type ContainerizedProbe struct {
	Markers func() bool
	Self    func(ctx context.Context) (string, error)
}

// Containerized reports whether the Instance is a Containerized Instance
// (runtime.DecideContainerized, #502). When it may be one, it asks the
// container runtime, connecting it if needed, for as long as ctx allows;
// without an answer --containerized and the marker files decide. The
// guess and any conflict are logged once (NoteContainerized).
func (e *Env) Containerized(ctx context.Context) bool {
	markers := e.markers()
	if !runtime.MayBeContainerized(e.Config.Containerized, markers) {
		return false
	}
	self, err := e.self(ctx)
	if err != nil {
		d := runtime.DecideContainerized(e.Config.Containerized, markers, false, "")
		if !errors.Is(err, ErrNoRuntime) {
			// The runtime may yet answer: not the no-socket guess.
			e.Log.Debug("container runtime not reachable; deciding the Containerized Instance without it", "err", err, "containerized", d.On)
			return d.On
		}
		e.NoteContainerized(d)
		return d.On
	}
	d := runtime.DecideContainerized(e.Config.Containerized, markers, true, self)
	e.NoteContainerized(d)
	return d.On
}

// NoteContainerized logs, once each, that d is a guess from the marker
// files or that --containerized disagrees with the runtime.
func (e *Env) NoteContainerized(d runtime.ContainerizedDecision) {
	if d.Guess {
		e.warnOnce(runtime.GuessWarning)
	}
	if d.Conflict != "" {
		e.warnOnce(d.Conflict)
	}
}

func (e *Env) warnOnce(msg string) {
	if _, done := e.containerizedWarned.LoadOrStore(msg, true); !done {
		e.Log.Warn(msg)
	}
}

func (e *Env) markers() bool {
	if e.ContainerizedProbe != nil {
		return e.ContainerizedProbe.Markers()
	}
	return runtime.InContainer()
}

// self asks the runtime for its self container, giving up when ctx ends
// even while another caller connects it.
func (e *Env) self(ctx context.Context) (string, error) {
	ask := func(ctx context.Context) (string, error) {
		if e.ContainerizedProbe != nil {
			return e.ContainerizedProbe.Self(ctx)
		}
		if e.Containers == nil {
			return "", ErrNoRuntime
		}
		rt, err := e.Containers.Runtime(ctx)
		if err != nil {
			return "", err
		}
		return rt.Self(), nil
	}
	type answer struct {
		self string
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		self, err := ask(ctx)
		ch <- answer{self, err}
	}()
	select {
	case a := <-ch:
		return a.self, a.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
