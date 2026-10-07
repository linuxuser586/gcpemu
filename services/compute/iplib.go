package compute

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// cidr is an IPv4 prefix in integer form.
type cidr struct {
	base uint32
	bits int
}

// parseCIDR parses an IPv4 CIDR. strict requires the address to be the
// network address (GCP rejects "10.0.0.1/24").
func parseCIDR(s string, strict bool) (cidr, error) {
	ip, n, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil || ip.To4() == nil {
		return cidr{}, fmt.Errorf("invalid IPv4 CIDR %q", s)
	}
	ones, _ := n.Mask.Size()
	c := cidr{base: ip4(n.IP), bits: ones}
	if strict && ip4(ip) != c.base {
		return cidr{}, fmt.Errorf("%q is not a network address", s)
	}
	return c, nil
}

func ip4(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }

func ipString(v uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return net.IP(b[:]).String()
}

// parseIP parses an IPv4 address to its integer form.
func parseIP(s string) (uint32, bool) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return 0, false
	}
	return ip4(ip), true
}

func (c cidr) size() uint64 { return uint64(1) << (32 - c.bits) }
func (c cidr) last() uint32 { return c.base + uint32(c.size()-1) }
func (c cidr) String() string {
	return fmt.Sprintf("%s/%d", ipString(c.base), c.bits)
}

// contains reports whether ip lies inside c.
func (c cidr) contains(ip uint32) bool { return ip >= c.base && ip <= c.last() }

// overlaps reports whether two prefixes share any address.
func (c cidr) overlaps(o cidr) bool { return c.base <= o.last() && o.base <= c.last() }

// covers reports whether c contains all of o.
func (c cidr) covers(o cidr) bool { return c.contains(o.base) && c.contains(o.last()) }

// gateway is the first host address (GCP's subnet gateway, .1).
func (c cidr) gateway() uint32 { return c.base + 1 }

// reservedTail is the second-to-last address, which GCP reserves for
// future use; the emulator's egress gateway lives there.
func (c cidr) reservedTail() uint32 { return c.last() - 1 }

// usable reports whether ip may be handed to a workload: not the network
// address, the gateway, GCP's reserved second-to-last address or the
// broadcast address (as in a GCP subnet).
func (c cidr) usable(ip uint32) bool {
	return c.contains(ip) && ip != c.base && ip != c.gateway() && ip != c.reservedTail() && ip != c.last()
}

// isPrivate reports whether c lies in a range GCP accepts for subnets
// without "privately used public IP" opt-in: RFC 1918, RFC 6598 shared
// space, and the other ranges GCP documents as valid.
func (c cidr) isPrivate() bool {
	for _, p := range privateRanges {
		if p.covers(c) {
			return true
		}
	}
	return false
}

var privateRanges = func() []cidr {
	var out []cidr
	for _, s := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
		c, _ := parseCIDR(s, true)
		out = append(out, c)
	}
	return out
}()
