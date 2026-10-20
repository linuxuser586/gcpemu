package emu

import (
	"context"
	"fmt"
	"strings"
)

// WaitState is how a Wait condition (see CONTEXT.md) stands.
type WaitState string

const (
	// WaitMet: the condition holds.
	WaitMet WaitState = "met"
	// WaitPending: the condition does not hold yet, but may.
	WaitPending WaitState = "pending"
	// WaitFailed: the condition can no longer hold.
	WaitFailed WaitState = "failed"
)

// WaitStatus is a Wait condition's state and, unless it is met, why.
type WaitStatus struct {
	State  WaitState `json:"state"`
	Reason string    `json:"reason,omitempty"`
}

// WaitTarget is a Wait condition's TARGET, PROJECT/LOCATION/NAME.
type WaitTarget struct{ Project, Location, Name string }

// WaitCondition evaluates one kind of Wait condition for a target. A
// resource that does not exist is pending, not failed: it may still be
// being created. An error means the target can never name a resource of
// the kind (e.g. a location the kind has none in).
type WaitCondition func(ctx context.Context, t WaitTarget) (WaitStatus, error)

// WaitConditioner is implemented by services that evaluate Wait conditions
// on their own resources (FR-CI-007, admin API /_emu/v1/conditions).
type WaitConditioner interface {
	// WaitConditions maps each kind the service evaluates, e.g.
	// "gke/cluster", to its WaitCondition.
	WaitConditions() map[string]WaitCondition
}

// ParseWaitCondition splits a Wait condition written KIND=TARGET, where
// TARGET is PROJECT/LOCATION/NAME. It does not check that KIND exists.
func ParseWaitCondition(s string) (kind string, t WaitTarget, err error) {
	kind, target, ok := strings.Cut(s, "=")
	if !ok || kind == "" {
		return "", WaitTarget{}, fmt.Errorf("Wait condition %q: want KIND=PROJECT/LOCATION/NAME", s)
	}
	parts := strings.Split(target, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", WaitTarget{}, fmt.Errorf("Wait condition %q: target %q is not PROJECT/LOCATION/NAME", s, target)
	}
	return kind, WaitTarget{Project: parts[0], Location: parts[1], Name: parts[2]}, nil
}
