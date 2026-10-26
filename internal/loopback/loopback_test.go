package loopback

import (
	"net"
	"testing"
)

func lo0(cidrs ...string) []net.Addr {
	var addrs []net.Addr
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		n.IP = ip
		addrs = append(addrs, n)
	}
	return addrs
}

// TestCheckAliases: aliases are Host aliases when each is a /32, and Wide
// when any has another netmask, which on macOS 27 keeps mDNSResponder busy
// (#677). 127.0.0.1's own /8 and IPv6 addresses don't count.
func TestCheckAliases(t *testing.T) {
	for name, tc := range map[string]struct {
		addrs []net.Addr
		want  AliasState
	}{
		"none":     {lo0("127.0.0.1/8", "::1/128", "fe80::1/64"), AliasesMissing},
		"empty":    {nil, AliasesMissing},
		"/32":      {lo0("127.0.0.1/8", "127.0.0.2/32", "127.0.0.3/32", "127.0.0.254/32", "::1/128"), AliasesHost},
		"/8":       {lo0("127.0.0.1/8", "127.0.0.2/8", "127.0.0.3/8"), AliasesWide},
		"one /8":   {lo0("127.0.0.1/8", "127.0.0.2/32", "127.0.0.3/8", "127.0.0.4/32"), AliasesWide},
		"/24":      {lo0("127.0.0.1/8", "127.0.0.2/24"), AliasesWide},
		"other IP": {lo0("127.0.0.1/8", "10.0.0.1/32"), AliasesMissing},
	} {
		if got := CheckAliases(tc.addrs); got != tc.want {
			t.Errorf("%s: CheckAliases = %v, want %v", name, got, tc.want)
		}
	}
}
