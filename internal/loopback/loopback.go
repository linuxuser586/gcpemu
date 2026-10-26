// Package loopback reports whether the host has loopback addresses beyond
// 127.0.0.1 (ADR 0005). Linux routes all of 127.0.0.0/8 to loopback; a
// macOS host has only 127.0.0.1 on lo0 until `gcpemu loopback install`
// adds aliases for 127.0.0.2 to 127.0.0.254.
package loopback

import (
	"fmt"
	"net"
)

// InstallHint is the command that adds the aliases on a host without them.
const InstallHint = "sudo gcpemu loopback install"

// Aliases reports whether 127.0.0.x addresses other than 127.0.0.1 can be
// bound, by binding 127.0.0.2:0.
func Aliases() bool {
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// AliasState is what lo0's addresses say about the aliases on macOS.
type AliasState int

const (
	// AliasesMissing: lo0 has no 127.0.0.x address other than 127.0.0.1.
	AliasesMissing AliasState = iota
	// AliasesHost: every alias is a /32, a host address of its own.
	AliasesHost
	// AliasesWide: some alias has another netmask. `ifconfig lo0 alias`
	// without one gives the classful /8, and 253 overlapping
	// 127.0.0.0/8s keep mDNSResponder at 100% CPU on macOS 27 (#677).
	AliasesWide
)

func (s AliasState) String() string {
	switch s {
	case AliasesMissing:
		return "missing"
	case AliasesHost:
		return "host"
	case AliasesWide:
		return "wide"
	}
	return fmt.Sprintf("AliasState(%d)", int(s))
}

// WideHint names the harm of AliasesWide.
const WideHint = "a /8 alias keeps mDNSResponder at 100% CPU on macOS 27 and slows every DNS lookup"

// CheckAliases reads the aliases from addrs, lo0's addresses.
func CheckAliases(addrs []net.Addr) AliasState {
	s := AliasesMissing
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip == nil || ip[0] != 127 || ip.Equal(net.IPv4(127, 0, 0, 1)) {
			continue
		}
		if ones, bits := n.Mask.Size(); bits == 0 || ones != bits {
			return AliasesWide
		}
		s = AliasesHost
	}
	return s
}

// Lo0Aliases is CheckAliases of the host's lo0. When lo0 can't be read it
// falls back to Aliases, which can't tell a netmask.
func Lo0Aliases() AliasState {
	if ifc, err := net.InterfaceByName("lo0"); err == nil {
		if addrs, err := ifc.Addrs(); err == nil {
			return CheckAliases(addrs)
		}
	}
	if Aliases() {
		return AliasesHost
	}
	return AliasesMissing
}
