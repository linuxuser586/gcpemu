// Package hostmode implements host mode (FR-CORE-043): host processes
// reach emulated services by their real hostnames (storage.googleapis.com,
// LOCATION-docker.pkg.dev, ...) once the local CA is trusted.
//
// # Design
//
// gcpemu runs unprivileged, so it can neither bind port 443 on the host
// nor edit /etc/hosts or the system resolver. Instead, `--host-mode`
// starts one small labelled container ("gcpemu-<id>-hostmode", image
// busybox running the gcpemu agent "host-frontend") on the instance's
// external network, which the Linux host reaches directly at the
// container's IP. The container:
//
//   - relays :443 to the emulator's Google frontend (internal/frontend),
//     which terminates TLS with a certificate from the instance CA for the
//     requested name and passes HTTP/1.1, HTTP/2 and gRPC to the gateway
//     (or the Artifact Registry listener for LOCATION-docker.pkg.dev);
//   - relays :80 to the gateway (plain HTTP with Host routing);
//   - serves DNS on :53: every hostname the gateway mounts and every
//     LOCATION-docker.pkg.dev resolves to the container's IP; other names
//     under the routed domains (googleapis.com, pkg.dev) go to the host's
//     real upstream resolvers (never back to systemd-resolved, which
//     would loop), everything else to the emulated Cloud DNS.
//
// The user then points name resolution at it, either per domain with
// systemd-resolved (`resolvectl dns <bridge> <ip>` and `resolvectl domain
// <bridge> ~googleapis.com ~pkg.dev`), or with the /etc/hosts block
// printed by `gcpemu hosts`, and trusts the CA with `gcpemu ca install`
// (or SSL_CERT_FILE from `gcpemu env --trust`). `gcpemu env` prints these
// instructions (to stderr) when host mode is on. On macOS and Windows the
// container IP is not routable from the host, so host mode is Linux-only.
package hostmode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Image runs the host-mode agent (any image works: the agent is a static
// binary). Pinned by tag; digest pinning is a release task.
const Image = "busybox:1.37"

// RoutedDomains are the DNS domains to route to the host-mode resolver.
var RoutedDomains = []string{"googleapis.com", "pkg.dev"}

// State values of Status.State.
const (
	StateDisabled = "disabled"
	StateStarting = "starting"
	StateReady    = "ready"
	StateError    = "error"
)

// Status is host mode's state, served at /_emu/v1/hostmode.
type Status struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	// IP is the frontend container's address (HTTPS :443, HTTP :80, DNS :53).
	IP string `json:"ip,omitempty"`
	// Interface is the host bridge interface the IP is reached through
	// (for systemd-resolved per-link DNS).
	Interface string `json:"interface,omitempty"`
	// Domains are the DNS domains to route to IP.
	Domains []string `json:"domains"`
	// Hosts are the names for an /etc/hosts block (exact names; registry
	// hosts expanded over every Artifact Registry location).
	Hosts []string `json:"hosts"`
	// CAFile is the CA to trust.
	CAFile string `json:"caFile,omitempty"`
}

// Manager runs the host-mode container.
type Manager struct {
	env   *emu.Env
	hosts func() []string

	mu     sync.Mutex
	status Status
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns a manager; hosts returns the gateway's mounted hosts.
func New(env *emu.Env, hosts func() []string) *Manager {
	m := &Manager{env: env, hosts: hosts}
	m.status = Status{Enabled: true, State: StateStarting, Domains: RoutedDomains}
	if env.CA != nil {
		m.status.CAFile = env.CA.Path()
	}
	return m
}

// Start brings the container up in the background; Status reports
// progress. It never fails instance start (NFR-REL-003).
func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancel, m.done = cancel, make(chan struct{})
	m.mu.Unlock()
	go func() {
		defer close(m.done)
		st, err := m.run(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			m.status.State, m.status.Error = StateError, err.Error()
			if ctx.Err() == nil {
				m.env.Log.Error("host mode unavailable", "err", err)
			}
			return
		}
		st.Enabled, st.State, st.Domains, st.CAFile = true, StateReady, RoutedDomains, m.status.CAFile
		m.status = st
		m.env.Log.Info("host mode ready: route DNS for googleapis.com and pkg.dev to "+st.IP+" (see `gcpemu env`), or install the block from `gcpemu hosts`",
			"ip", st.IP, "interface", st.Interface)
	}()
}

// Stop cancels a pending start. The container is removed with the
// instance's other containers.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Status returns the current state.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{State: StateDisabled, Domains: RoutedDomains}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status
	st.Hosts = HostsBlockNames(m.hosts())
	return st
}

// WaitReady waits until host mode is ready or failed.
func (m *Manager) WaitReady(ctx context.Context) (Status, error) {
	for {
		st := m.Status()
		switch st.State {
		case StateReady:
			return st, nil
		case StateError:
			return st, errors.New(st.Error)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// HostsBlockNames returns the names an /etc/hosts block must list: the
// mounted *.googleapis.com hosts and LOCATION-docker.pkg.dev for every
// Artifact Registry location. Other mounted names (accounts.google.com)
// are left out so that host browsers keep reaching Google.
func HostsBlockNames(mounted []string) []string {
	var out []string
	for _, h := range mounted {
		if strings.HasSuffix(h, ".googleapis.com") {
			out = append(out, h)
		}
	}
	locs := []string{"us", "europe", "asia"}
	for _, r := range locations.Regions() {
		locs = append(locs, r.Name)
	}
	for _, l := range locs {
		out = append(out, l+"-docker.pkg.dev")
	}
	sort.Strings(out)
	return out
}

// run creates the container and waits until it answers.
func (m *Manager) run(ctx context.Context) (Status, error) {
	if m.env.Containers == nil {
		return Status{}, errors.New("host mode needs a container runtime (Docker or Podman)")
	}
	np, err := m.env.Containers.Netplane(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("host mode needs a container runtime (Docker or Podman): %w", err)
	}
	rt := np.Runtime()
	ext, err := np.External(ctx)
	if err != nil {
		return Status{}, err
	}
	svc, err := np.Services(ctx)
	if err != nil {
		return Status{}, err
	}
	cfg := agentConfig{AnswerCIDR: ext.Subnet, Hosts: m.hosts(), Resolvers: upstreamResolvers(m.env.Config.Offline)}
	if cfg.Frontend, err = np.Addr(ctx, frontend.Endpoint); err != nil {
		return Status{}, err
	}
	if cfg.Gateway, err = np.Addr(ctx, "gateway"); err != nil {
		return Status{}, err
	}
	if m.env.Endpoints.Get("dns") != "" {
		cfg.DNS, _ = np.Addr(ctx, "dns")
	}
	if err := rt.EnsureImage(ctx, Image, m.env.Config.Offline); err != nil {
		return Status{}, fmt.Errorf("host mode image %s: %w", Image, err)
	}
	bin, err := agent.Binary()
	if err != nil {
		return Status{}, err
	}
	name := rt.Name("hostmode")
	if err := rt.RemoveContainer(ctx, name, true); err != nil {
		return Status{}, err
	}
	cfgJSON, _ := json.Marshal(cfg)
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      Image,
		Entrypoint: []string{agent.ContainerPath},
		Env:        []string{agent.EnvVar + "=" + agentName, configEnv + "=" + string(cfgJSON)},
		Labels:     rt.Labels("core", "hostmode", "frontend"),
		Hostname:   "gcpemu-hostmode",
		Networks:   []runtime.Attachment{{Network: ext.Name}, {Network: svc.Name}},
		Init:       true,
	})
	if err != nil {
		return Status{}, err
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		return Status{}, err
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		return Status{}, err
	}
	if err := rt.WaitRunning(ctx, id); err != nil {
		return Status{}, err
	}
	ct, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return Status{}, err
	}
	ip := ct.IPs[ext.Name]
	if ip == "" {
		return Status{}, fmt.Errorf("host-mode container has no address on %s", ext.Name)
	}
	if err := waitDNS(ctx, ip, 20*time.Second); err != nil {
		logs, _ := rt.Logs(context.Background(), id, 20)
		return Status{}, fmt.Errorf("host-mode container did not answer DNS on %s: %w\n%s", ip, err, logs)
	}
	return Status{IP: ip, Interface: interfaceWith(ext.Gateway)}, nil
}

// waitDNS polls the container's resolver for a served name.
func waitDNS(ctx context.Context, ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	c := &mdns.Client{Timeout: time.Second}
	m := new(mdns.Msg)
	m.SetQuestion("metadata.gcpemu.invalid.", mdns.TypeTXT)
	var err error
	for time.Now().Before(deadline) {
		if _, _, err = c.ExchangeContext(ctx, m, net.JoinHostPort(ip, "53")); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return err
}

// interfaceWith returns the host interface holding ip ("" if none).
func interfaceWith(ip string) string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.String() == ip {
				return i.Name
			}
		}
	}
	return ""
}

// upstreamResolvers returns the host's real DNS servers for names under
// the routed domains that the emulator does not serve: systemd-resolved's
// upstream list if present, else non-loopback servers from
// /etc/resolv.conf, else public resolvers unless offline.
func upstreamResolvers(offline bool) []string {
	var out []string
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		cc, err := mdns.ClientConfigFromFile(f)
		if err != nil {
			continue
		}
		for _, s := range cc.Servers {
			if ip := net.ParseIP(s); ip != nil && !ip.IsLoopback() && ip.To4() != nil {
				out = append(out, net.JoinHostPort(s, cc.Port))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if offline {
		return nil
	}
	return []string{"8.8.8.8:53", "1.1.1.1:53"}
}
