package instance

import (
	"context"
	"sort"
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

// ContainerInfo describes one container the instance owns (admin API,
// `gcpemu status`).
type ContainerInfo struct {
	Name     string `json:"name"`
	Service  string `json:"service,omitempty"`
	Resource string `json:"resource,omitempty"`
	Role     string `json:"role,omitempty"`
	Image    string `json:"image"`
	State    string `json:"state"`
}

// list returns the instance's labelled containers, sorted by name. It does
// not connect to the runtime if no service has used it yet (nothing can
// be running then).
func (c *containers) list(ctx context.Context) ([]ContainerInfo, error) {
	c.mu.Lock()
	rt := c.rt
	c.mu.Unlock()
	out := []ContainerInfo{}
	if rt == nil {
		return out, nil
	}
	cts, err := rt.ListContainers(ctx, map[string]string{runtime.LabelInstance: rt.InstanceID})
	if err != nil {
		return nil, err
	}
	for _, ct := range cts {
		out = append(out, ContainerInfo{Name: ct.Name, Service: ct.Labels[runtime.LabelService],
			Resource: ct.Labels[runtime.LabelResource], Role: ct.Labels[runtime.LabelRole], Image: ct.Image, State: ct.Status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Containers lists the instance's containers (see containers.list).
func (in *Instance) Containers(ctx context.Context) ([]ContainerInfo, error) {
	return in.containers.list(ctx)
}
