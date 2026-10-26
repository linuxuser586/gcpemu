package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
)

// Manager scopes runtime objects to one emulator instance: it names and
// labels them, and reclaims orphans left by a crashed run (FR-CORE-006).
type Manager struct {
	*Client
	Info       Info
	InstanceID string
	Instance   string
	// Dir is the instance's absolute data dir, recorded on every object
	// (LabelDir) so that an Abandoned Instance ID can be told apart.
	Dir     string
	Offline bool
	// Ephemeral instances also delete volumes and networks on cleanup.
	Ephemeral bool
	Log       *slog.Logger
	// Reserved, when set, returns the addresses that stored resources have
	// recorded and ask for again when their containers are recreated (a
	// Cloud SQL instance's public IP). See CreateContainer.
	Reserved func() []string
	// Agent finds the linux gcpemu binary for Managed containers; nil
	// resolves without the cache or the published image (AgentBinary).
	Agent *agent.Resolver
	// VM says whether a Containerized Instance is on a VM runtime, whose
	// host reaches it only through its published ports (ADR 0012).
	VM VMDecision
	// LoopbackAliases says whether, on a VM runtime, Cloud SQL instances
	// may publish their ports on 127.0.0.2-127.0.0.254 (ADR 0012).
	LoopbackAliases AliasDecision

	addrMu    sync.Mutex
	agentOnce sync.Once
}

// AgentBinary returns the path of a static linux gcpemu binary for the
// runtime's architecture, to copy into Managed containers.
func (m *Manager) AgentBinary(ctx context.Context) (string, error) {
	m.agentOnce.Do(func() {
		if m.Agent == nil {
			m.Agent = &agent.Resolver{Arch: m.Info.Arch}
		}
	})
	return m.Agent.Binary(ctx)
}

// pickAttempts bounds how many addresses CreateContainer and Connect pick
// for an attachment the runtime refuses at the address picked.
const pickAttempts = 3

// CreateContainer creates a container like Client.CreateContainer, except
// that an attachment without a fixed address, on a network whose subnet
// holds a Reserved address, gets the lowest free address outside the
// reserved set. The runtime would otherwise hand out a reserved address
// whose container is not running yet, e.g. after a restart (withPicks).
func (m *Manager) CreateContainer(ctx context.Context, s ContainerSpec) (string, error) {
	reserved := m.reserved()
	var dynamic []int // attachments without a fixed address
	var networks []string
	for i, a := range s.Networks {
		if a.IP == "" {
			dynamic, networks = append(dynamic, i), append(networks, a.Network)
		}
	}
	if len(reserved) == 0 || len(dynamic) == 0 {
		return m.Client.CreateContainer(ctx, s)
	}
	var id string
	err := m.withPicks(ctx, networks, reserved, func(ips []string) error {
		picked := s
		picked.Networks = slices.Clone(s.Networks)
		for j, i := range dynamic {
			picked.Networks[i].IP = ips[j]
		}
		var err error
		id, err = m.Client.CreateContainer(ctx, picked)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", s.Name, err)
	}
	return id, nil
}

// Connect connects container to network like Client.Connect, except that
// without a fixed address, on a network whose subnet holds a Reserved
// address, it takes an address as CreateContainer picks one.
func (m *Manager) Connect(ctx context.Context, network, container, ip string, aliases ...string) error {
	reserved := m.reserved()
	if ip != "" || len(reserved) == 0 {
		return m.Client.Connect(ctx, network, container, ip, aliases...)
	}
	return m.withPicks(ctx, []string{network}, reserved, func(ips []string) error {
		return m.Client.Connect(ctx, network, container, ips[0], aliases...)
	})
}

// withPicks calls try with an address for each of networks (freeAddr; ""
// where the runtime picks), and again with the next free ones while the
// runtime refuses them, up to pickAttempts times. It never leaves the
// choice to the runtime on a network that holds a reserved address, which
// the runtime could hand out (ADR 0035). Picks are serialized so that the
// next one sees this container's address.
func (m *Manager) withPicks(ctx context.Context, networks []string, reserved []netip.Addr, try func(ips []string) error) error {
	m.addrMu.Lock()
	defer m.addrMu.Unlock()
	var err error
	for range pickAttempts {
		ips := make([]string, len(networks))
		var picked []netip.Addr
		for i, n := range networks {
			if ips[i], err = m.freeAddr(ctx, n, reserved); err != nil {
				return err
			}
			if ip, perr := netip.ParseAddr(ips[i]); perr == nil {
				picked = append(picked, ip)
			}
		}
		if err = try(ips); err == nil || len(picked) == 0 || IsAlreadyExists(err) {
			return err
		}
		reserved = append(slices.Clip(reserved), picked...)
	}
	return err
}

// reserved parses the Reserved addresses.
func (m *Manager) reserved() []netip.Addr {
	var out []netip.Addr
	if m.Reserved != nil {
		for _, a := range m.Reserved() {
			if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
				out = append(out, ip)
			}
		}
	}
	return out
}

// freeAddr returns the lowest address of network that is not reserved,
// held or the gateway, or "" (the runtime picks) when no reserved address
// lies in the network's subnet.
func (m *Manager) freeAddr(ctx context.Context, network string, reserved []netip.Addr) (string, error) {
	n, err := m.InspectNetwork(ctx, network)
	if err != nil {
		return "", err
	}
	prefix, err := netip.ParsePrefix(n.Subnet)
	if err != nil || !prefix.Addr().Is4() || !slices.ContainsFunc(reserved, prefix.Contains) {
		return "", nil
	}
	held, err := m.NetworkAddrs(ctx, network)
	if err != nil {
		return "", err
	}
	for _, ip := range reserved {
		held[ip.String()] = true
	}
	held[n.Gateway] = true
	prefix = prefix.Masked()
	for ip := prefix.Addr().Next(); prefix.Contains(ip.Next()); ip = ip.Next() {
		if !held[ip.String()] {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("network %s has no free address", network)
}

// Labels returns the standard label set for an object.
func (m *Manager) Labels(service, resource, role string) map[string]string {
	l := map[string]string{LabelInstance: m.InstanceID}
	if m.Dir != "" {
		l[LabelDir] = m.Dir
	}
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
