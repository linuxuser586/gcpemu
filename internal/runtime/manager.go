package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Manager scopes runtime objects to one emulator instance: it names and
// labels them, and reclaims orphans left by a crashed run (FR-CORE-006).
type Manager struct {
	*Client
	Info       Info
	InstanceID string
	Instance   string
	Offline    bool
	// Ephemeral instances also delete volumes and networks on cleanup.
	Ephemeral bool
	Log       *slog.Logger
}

// Labels returns the standard label set for an object.
func (m *Manager) Labels(service, resource, role string) map[string]string {
	l := map[string]string{LabelInstance: m.InstanceID}
	if service != "" {
		l[LabelService] = service
	}
	if resource != "" {
		l[LabelResource] = resource
	}
	if role != "" {
		l[LabelRole] = role
	}
	return l
}

var nameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// Name builds a runtime object name "gcpemu-<id>-<parts...>".
func (m *Manager) Name(parts ...string) string {
	s := "gcpemu-" + m.InstanceID
	for _, p := range parts {
		s += "-" + strings.Trim(nameUnsafe.ReplaceAllString(p, "-"), "-")
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// Reclaim removes every container this instance ID owns (orphans from a
// previous run). For ephemeral instances it also removes networks and
// volumes; inside a container the emulator rejoins the networks kept.
func (m *Manager) Reclaim(ctx context.Context) error {
	err := m.cleanup(ctx, m.Ephemeral)
	if m.Self() == "" || m.Ephemeral {
		return err
	}
	nets, lerr := m.ListNetworks(ctx, map[string]string{LabelInstance: m.InstanceID})
	errs := []error{err, lerr}
	for _, n := range nets {
		errs = append(errs, m.AttachSelf(ctx, n.Name))
	}
	return errors.Join(errs...)
}

// Cleanup removes all of the instance's containers; with all=true also its
// networks and volumes.
func (m *Manager) Cleanup(ctx context.Context, all bool) error { return m.cleanup(ctx, all) }

func (m *Manager) cleanup(ctx context.Context, all bool) error {
	sel := map[string]string{LabelInstance: m.InstanceID}
	cs, err := m.ListContainers(ctx, sel)
	if err != nil {
		return err
	}
	var errs []error
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, c := range cs {
		wg.Add(1)
		go func(id, name string) {
			defer wg.Done()
			if err := m.RemoveContainer(ctx, id, true); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("remove container %s: %w", name, err))
				mu.Unlock()
			}
		}(c.ID, c.Name)
	}
	wg.Wait()
	if !all {
		return errors.Join(errs...)
	}
	nets, err := m.ListNetworks(ctx, sel)
	if err != nil {
		errs = append(errs, err)
	}
	for _, n := range nets {
		if err := m.RemoveNetwork(ctx, n.ID); err != nil {
			errs = append(errs, fmt.Errorf("remove network %s: %w", n.Name, err))
		}
	}
	vols, err := m.ListVolumes(ctx, sel)
	if err != nil {
		errs = append(errs, err)
	}
	for _, v := range vols {
		if err := m.RemoveVolume(ctx, v); err != nil {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", v, err))
		}
	}
	return errors.Join(errs...)
}

// WaitRunning polls until the container is running or ctx ends. It fails
// early if the container exits.
func (m *Manager) WaitRunning(ctx context.Context, id string) error {
	for {
		c, err := m.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		if c.Running {
			return nil
		}
		if c.Status == "exited" || c.Status == "dead" {
			logs, _ := m.Logs(ctx, id, 20)
			return fmt.Errorf("container %s exited with code %d: %s", c.Name, c.ExitCode, strings.TrimSpace(logs))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
