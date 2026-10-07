package frontend

import (
	mdns "github.com/miekg/dns"
)

// NetworkOption is the EDNS0 local option (RFC 6891 private range) with
// which a VPC's DNS relay tells the emulated Cloud DNS the VPC network a
// query comes from ("projects/P/global/networks/N"), so that private zones
// are answered only on the networks they are bound to (FR-DNS-004).
const NetworkOption = 65400

// SetNetwork returns a copy of m that carries network in NetworkOption.
func SetNetwork(m *mdns.Msg, network string) *mdns.Msg {
	c := m.Copy()
	opt := c.IsEdns0()
	if opt == nil {
		c.SetEdns0(mdns.MinMsgSize, false)
		opt = c.IsEdns0()
	}
	kept := opt.Option[:0]
	for _, o := range opt.Option {
		if o.Option() != NetworkOption {
			kept = append(kept, o)
		}
	}
	opt.Option = append(kept, &mdns.EDNS0_LOCAL{Code: NetworkOption, Data: []byte(network)})
	return c
}

// Network returns the VPC network a query carries in NetworkOption, or "".
func Network(m *mdns.Msg) string {
	opt := m.IsEdns0()
	if opt == nil {
		return ""
	}
	for _, o := range opt.Option {
		if l, ok := o.(*mdns.EDNS0_LOCAL); ok && l.Code == NetworkOption {
			return string(l.Data)
		}
	}
	return ""
}
