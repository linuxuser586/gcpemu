// Package loopback reports whether the host has loopback addresses beyond
// 127.0.0.1 (ADR 0005), and on macOS whether each is a /32. Linux routes all of 127.0.0.0/8 to loopback; a
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
	// AliasesNarrow: every loopback alias is a /32.
	AliasesNarrow
	// AliasesWide: some loopback alias has another netmask. `ifconfig lo0 alias`
	// without one gives the classful /8, and 253 overlapping
	// 127.0.0.0/8s keep mDNSResponder at 100% CPU on macOS 27 (#677).
	AliasesWide
)

func (s AliasState) String() string {
	switch s {
	case AliasesMissing:
		return "missing"
	case AliasesNarrow:
		return "narrow"
	case AliasesWide:
		return "wide"
	}
	return fmt.Sprintf("AliasState(%d)", int(s))
}

// WideHint names the harm of AliasesWide.
const WideHint = "a /8 loopback alias keeps mDNSResponder at 100% CPU on macOS 27 and slows every DNS lookup"

// CheckAliases reads the aliases from addrs, lo0's addresses.
func CheckAliases(addrs []net.Addr) AliasState {
	s := AliasesMissing
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip == nil || ip[0] != 127 || ip[1] != 0 || ip[2] != 0 || ip[3] == 1 {
			continue
		}
		if ones, bits := n.Mask.Size(); bits == 0 || ones != bits {
			return AliasesWide
		}
		s = AliasesNarrow
	}
	return s
}

// Lo0Aliases is CheckAliases of the host's lo0, AliasesMissing when lo0
// can't be read.
func Lo0Aliases() AliasState {
	ifc, err := net.InterfaceByName("lo0")
	if err != nil {
		return AliasesMissing
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return AliasesMissing
	}
	return CheckAliases(addrs)
}
