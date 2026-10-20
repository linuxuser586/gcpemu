package emu

import (
	"context"

	"github.com/linuxuser586/gcpemu/internal/lanbind"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// LANBind reports where the Instance binds the --lb-bind, --gke-bind,
// --ar-bind and --gfe-bind addresses. It asks the container runtime only
// in a container, connecting it if needed.
func (e *Env) LANBind(ctx context.Context) lanbind.Where {
	if e.LANBindWhere != nil {
		return e.LANBindWhere(ctx)
	}
	if !runtime.InContainer() {
		return lanbind.Host
	}
	if e.Containers == nil {
		return lanbind.Unconfirmed
	}
	rt, err := e.Containers.Runtime(ctx)
	if err != nil {
		return lanbind.Unconfirmed
	}
	return lanbind.Locate(true, true, rt.Self())
}

// ExplainBind is lanbind.Explain for err, the failure to bind a --*-bind
// address; it locates the Instance only when err needs it.
func (e *Env) ExplainBind(ctx context.Context, err error) error {
	if !lanbind.NotLocal(err) {
		return err
	}
	return lanbind.Explain(e.LANBind(ctx), err)
}
