package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
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

// AttachSelf connects the emulator's container to network; it is a no-op
// on the host and when already connected.
func (c *Client) AttachSelf(ctx context.Context, network string) error {
	if c.self == "" {
		return nil
	}
	// GCP reserves the second-to-last address of a subnet, so no resource
	// the emulator places on a VPC subnetwork can take it.
	ip := ""
	if n, err := c.InspectNetwork(ctx, network); err == nil {
		ip = secondToLast(n.Subnet)
	}
	err := c.Connect(ctx, network, c.self, ip)
	if err != nil && ip != "" && !alreadyConnected(err) {
		err = c.Connect(ctx, network, c.self, "")
	}
	if alreadyConnected(err) {
		return nil
	}
	return err
}

func alreadyConnected(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.Status == http.StatusConflict || strings.Contains(ae.Message, "already exists"))
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
