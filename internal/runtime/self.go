package runtime

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// The emulator inside a container (the OCI image with the host's runtime
// socket mounted, ADR 0003). Its network namespace is not the host's, so
// the bridge networks it creates are not reachable from it, and Managed
// containers cannot reach it on a bridge gateway. Detect finds the
// emulator's own container; the client then connects it to every network
// it creates, and callers address Managed containers, and are addressed
// by them, on those networks (SelfIP). An emulator on the host network
// (`--network host`) behaves as on the host.

// SelfEnv names the emulator's own container explicitly.
const SelfEnv = "GCPEMU_SELF_CONTAINER"

var mountContainerID = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// selfCandidates lists names that may identify the current container.
func selfCandidates() []string {
	if v := os.Getenv(SelfEnv); v != "" {
		return []string{v}
	}
	if !fileExists("/.dockerenv") && !fileExists("/run/.containerenv") {
		return nil
	}
	var out []string
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if m := mountContainerID.FindSubmatch(b); m != nil {
			out = append(out, string(m[1]))
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		out = append(out, h)
	}
	return out
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// detectSelf records the emulator's own container unless it shares the
// host's network namespace.
func (c *Client) detectSelf(ctx context.Context) {
	for _, id := range selfCandidates() {
		var v struct {
			ID         string `json:"Id"`
			HostConfig struct{ NetworkMode string }
		}
		if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &v); err != nil {
			continue
		}
		c.selfFound = true
		if v.HostConfig.NetworkMode == "host" {
			return
		}
		c.self = v.ID
		return
	}
}

// Self returns the emulator's own container ID, or "" when it runs on the
// host (or on the host network).
func (c *Client) Self() string { return c.self }

// SelfFound reports whether the runtime found the emulator's own
// container, on the host's network or not. On the host, or in a container
// it can't find, Self is "" too (#502).
func (c *Client) SelfFound() bool { return c.selfFound }

// AttachSelf connects the emulator's container to network; it is a no-op
// on the host and when already connected.
func (c *Client) AttachSelf(ctx context.Context, network string) error {
	if c.self == "" {
		return nil
	}
	_, err := c.attach(ctx, network, c.self, true, "")
	return err
}

// SetMember makes container join every network the client creates from
// now on, as the emulator's own container does, and leave each network
// before it is removed. joined is called with true after each join and
// with false before each removal. The VM relay (ADR 0005) is the member;
// "" ends membership.
func (c *Client) SetMember(container string, joined func(n Network, in bool)) {
	c.memberMu.Lock()
	defer c.memberMu.Unlock()
	c.member, c.joined = container, joined
}

// SetMemberAddr has the member container take, on each network it joins
// without reserved, the address addr returns for that network while it
// is free, and else chooses as without a pin; "" leaves the choice to
// AttachMember. The VM relay holds the
// Host address of a VPC container network this way, which must not move
// when the relay or the Instance restarts (ADR 0014).
func (c *Client) SetMemberAddr(addr func(network string) string) {
	c.memberMu.Lock()
	defer c.memberMu.Unlock()
	c.memberAddr = addr
}

// AttachMember connects the member container (SetMember) to network; it is
// a no-op without a member and when already connected. With reserved, it
// takes the subnet's second-to-last address as AttachSelf does. Without,
// it takes its pinned address (SetMemberAddr), or else the highest free
// address below the second-to-last one: on a VPC subnetwork the
// second-to-last address is the NAT egress proxy's, and compute hands out
// addresses from the bottom, possibly before their containers start, and
// none that a container holds (#146).
func (c *Client) AttachMember(ctx context.Context, network string, reserved bool) error {
	c.memberMu.Lock()
	member, joined, addr := c.member, c.joined, c.memberAddr
	c.memberMu.Unlock()
	if member == "" {
		return nil
	}
	pinned := ""
	if addr != nil && !reserved {
		pinned = addr(network)
	}
	n, err := c.attach(ctx, network, member, reserved, pinned)
	if err != nil {
		return err
	}
	joined(n, true)
	return nil
}

// attach connects container to network, with reserved at the subnet's
// second-to-last address when it is free: GCP reserves that address, so
// no resource the emulator places on a VPC subnetwork can take it.
// Otherwise it asks for pinned, or the highest free address without one.
func (c *Client) attach(ctx context.Context, network, container string, reserved bool, pinned string) (Network, error) {
	n, ierr := c.InspectNetwork(ctx, network)
	ip := ""
	switch {
	case ierr != nil:
	case reserved:
		ip = secondToLast(n.Subnet)
	case pinned != "":
		ip = pinned
	default:
		ip = c.highestFree(ctx, n)
	}
	err := c.Connect(ctx, network, container, ip)
	if err != nil && pinned != "" && ip == pinned && !IsAlreadyExists(err) {
		// Taken, or outside a recreated network's subnet: as unpinned.
		if ip = c.highestFree(ctx, n); ip != "" {
			err = c.Connect(ctx, network, container, ip)
		}
	}
	if err != nil && ip != "" && !IsAlreadyExists(err) {
		err = c.Connect(ctx, network, container, "")
	}
	if IsAlreadyExists(err) {
		err = nil
	}
	return n, err
}

// highestFree returns the highest address of n below its second-to-last
// one that no container holds and that is not the gateway, or "".
func (c *Client) highestFree(ctx context.Context, n Network) string {
	top, err := netip.ParseAddr(secondToLast(n.Subnet))
	if err != nil {
		return ""
	}
	p, _ := netip.ParsePrefix(n.Subnet)
	held, err := c.NetworkAddrs(ctx, n.Name)
	if err != nil {
		return ""
	}
	for a := top.Prev(); a.IsValid() && p.Contains(a) && a != p.Masked().Addr(); a = a.Prev() {
		if !held[a.String()] && a.String() != n.Gateway {
			return a.String()
		}
	}
	return ""
}

// secondToLast returns the second-to-last address of an IPv4 subnet.
func secondToLast(subnet string) string {
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() || p.Bits() > 29 {
		return ""
	}
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= (1 << (32 - p.Bits())) - 1 // broadcast
	v--
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()
}

// SelfIP returns the emulator's address on network ("" on the host).
func (c *Client) SelfIP(ctx context.Context, network string) (string, error) {
	if c.self == "" {
		return "", nil
	}
	ct, err := c.InspectContainer(ctx, c.self)
	if err != nil {
		return "", err
	}
	if ip := ct.IPs[network]; ip != "" {
		return ip, nil
	}
	if err := c.AttachSelf(ctx, network); err != nil {
		return "", err
	}
	if ct, err = c.InspectContainer(ctx, c.self); err != nil {
		return "", err
	}
	return ct.IPs[network], nil
}

// SelfPorts maps each TCP port the emulator's own container publishes to
// the host address it is published at (127.0.0.1 for the wildcard), or is
// nil on the host. On a VM runtime these are the only container ports
// the host reaches (ADR 0012).
func (c *Client) SelfPorts(ctx context.Context) (map[int]string, error) {
	if c.self == "" {
		return nil, nil
	}
	ct, err := c.InspectContainer(ctx, c.self)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for k, addr := range ct.Ports {
		p, proto, _ := strings.Cut(k, "/")
		if n, err := strconv.Atoi(p); err == nil && proto == "tcp" {
			out[n] = addr
		}
	}
	return out, nil
}

// InContainer reports, without asking the runtime, whether the emulator
// may run in a container: GCPEMU_SELF_CONTAINER is set, or the container
// marker files exist.
func InContainer() bool { return selfCandidates() != nil }
