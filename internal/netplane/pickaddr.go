package netplane

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
)

// PickAddr chooses a fixed address for a container on n: preferred while
// it is a host address inside n's subnet, otherwise a random host address
// in the upper half of the subnet, where it rarely meets the addresses the
// runtime hands out from the bottom of the range. The runtime may still
// refuse it as taken; the caller then picks again.
func (n Net) PickAddr(preferred string) (string, error) {
	p, err := netip.ParsePrefix(n.Subnet)
	if err != nil || !p.Addr().Is4() || p.Bits() > 30 {
		return "", fmt.Errorf("network %s: no IPv4 subnet to pick an address from (%q)", n.Name, n.Subnet)
	}
	p = p.Masked()
	size := uint32(1) << (32 - p.Bits())
	base := v4(p.Addr())
	host := func(a netip.Addr) bool {
		off := v4(a) - base
		return a.Is4() && p.Contains(a) && off != 0 && off != size-1 && a.String() != n.Gateway
	}
	if a, err := netip.ParseAddr(preferred); err == nil && host(a) {
		return a.String(), nil
	}
	half := size / 2
	for range 64 {
		a := addr4(base + half + rand.Uint32N(size-half))
		if host(a) {
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("network %s: no free host address in the upper half of %s", n.Name, n.Subnet)
}

func v4(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func addr4(u uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)})
}
