package instance

import (
	"context"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// containers lazily connects to the container runtime the first time a
// service needs it, reclaiming orphans of a previous run (FR-CORE-006).
type containers struct {
	in *Instance

	mu    sync.Mutex
	rt    *runtime.Manager
	plane *netplane.Plane
	err   error
}

func (c *containers) Runtime(ctx context.Context) (*runtime.Manager, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rt != nil || c.err != nil {
		return c.rt, c.err
	}
	cl, err := runtime.Detect()
	if err != nil {
		c.err = err
		return nil, err
	}
	info, err := cl.Info(ctx)
	if err != nil {
		return nil, err // transient; retry next time
	}
	m := &runtime.Manager{
		Client: cl, Info: info, InstanceID: c.in.ID, Instance: c.in.Config.Instance,
		Offline: c.in.Config.Offline, Ephemeral: c.in.Config.Ephemeral,
		Log: c.in.Env.Log.With("component", "runtime"),
	}
	if err := m.Reclaim(ctx); err != nil {
		m.Log.Warn("reclaiming orphaned containers", "err", err)
	}
	m.Log.Info("container runtime connected", "runtime", info.Name, "version", info.Version, "endpoint", cl.Endpoint)
	c.rt = m
	return m, nil
}

func (c *containers) Netplane(ctx context.Context) (*netplane.Plane, error) {
	rt, err := c.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.plane == nil {
		c.plane = netplane.New(rt, c.in.Env.Endpoints.All, c.in.Env.Log.With("component", "netplane"))
	}
	return c.plane, nil
}

// shutdown removes the instance's containers (and, for ephemeral instances,
// networks and volumes) after services have stopped.
func (c *containers) shutdown() error {
	c.mu.Lock()
	rt, plane := c.rt, c.plane
	c.mu.Unlock()
	if plane != nil {
		plane.Close()
	}
	if rt == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return rt.Cleanup(ctx, c.in.Config.Ephemeral)
}
