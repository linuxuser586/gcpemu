package emu

import (
	"context"

	"github.com/linuxuser586/gcpemu/internal/lanbind"
)

// LANBind reports where the Instance binds the --lb-bind, --gke-bind,
// --ar-bind, --gfe-bind, --sql-bind and --link-bind addresses: in its own
// network namespace only in a Containerized Instance (Containerized's
// decision, without logging), which is Bridge only when the runtime found
// its container (#564). It
// asks the container runtime only when the Instance may be one,
// connecting it if needed.
func (e *Env) LANBind(ctx context.Context) lanbind.Where {
	if e.LANBindWhere != nil {
		return e.LANBindWhere(ctx)
	}
	d, confirmed, _ := e.decideContainerized(ctx)
	return lanbind.Locate(d.On, confirmed)
}

// ExplainBind is lanbind.Explain for err, the failure to bind a --*-bind
// address; it locates the Instance only when err needs it.
func (e *Env) ExplainBind(ctx context.Context, err error) error {
	if !lanbind.NotLocal(err) {
		return err
	}
	return lanbind.Explain(e.LANBind(ctx), err)
}
