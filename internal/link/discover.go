package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/mdns"
)

// ServiceType is the DNS-SD service type Instances advertise their
// --link-bind listeners under.
const ServiceType = "_gcpemu._tcp"

// Sources of a Discovered Instance.
const (
	SourceMDNS   = "mdns"
	SourceStatic = "static"
)

// TXT record keys an Instance advertises.
const (
	txtName        = "name"
	txtID          = "id"
	txtVersion     = "version"
	txtMinVersion  = "minversion"
	txtFingerprint = "fingerprint"
)

// Discovered is a Discovered Instance: one found on the LAN because it
// advertises itself, or at a static link address, which `gcpemu link pair`
// takes by Name. Discovery never pairs it.
type Discovered struct {
	// Name is "<Instance name>@<host>", host being the machine it
	// advertises from, or the static address's.
	Name       string `json:"name"`
	Instance   string `json:"instance"`
	InstanceID string `json:"instanceId"`
	// Address is its --link-bind listener, host:port.
	Address string `json:"address"`
	// Fingerprint is the Instance CA fingerprint it advertises, or
	// presented at a static address. Pairing checks it again.
	Fingerprint string `json:"fingerprint"`
	Version     int    `json:"version"`
	MinVersion  int    `json:"minVersion"`
	Source      string `json:"source"`
	// Link names this Instance's Link to it, when they are paired.
	Link string `json:"link,omitempty"`
	// Problem says why it cannot be paired as it is: an incompatible link
	// protocol version, a CA other than its Link's, or, at a static
	// address, that it cannot be reached.
	Problem string `json:"problem,omitempty"`
}

// Advertise advertises the link listeners at addrs (host:port) over
// mDNS/DNS-SD, until Close. Only an Instance with --link-bind calls it
// (ADR 0027).
func (m *Manager) Advertise(addrs []string) error {
	if len(addrs) == 0 {
		return errors.New("no link listener to advertise")
	}
	var ips []net.IP
	port := 0
	for _, a := range addrs {
		host, p, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
		}
		port, _ = strconv.Atoi(p)
	}
	v := m.cfg.Versions
	svc := mdns.Service{
		Instance: m.cfg.Instance + "@" + hostLabel(), Type: ServiceType, Host: "gcpemu-" + m.cfg.ID, Port: port, Addrs: ips,
		TXT: []string{
			"txtvers=1", txtName + "=" + m.cfg.Instance, txtID + "=" + m.cfg.ID,
			txtVersion + "=" + strconv.Itoa(v.Max), txtMinVersion + "=" + strconv.Itoa(v.Min),
			txtFingerprint + "=" + m.fingerprint(),
		},
	}
	o := m.cfg.MDNS
	o.Log = m.cfg.Log
	r, err := mdns.Advertise(svc, o)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return r.Close()
	}
	m.responders = append(m.responders, r)
	m.mu.Unlock()
	m.cfg.Log.Info("advertised over mDNS for `gcpemu link discover`", "name", svc.Instance, "addresses", addrs)
	return nil
}

// hostLabel is this machine's host name up to its first dot, lowercased.
func hostLabel() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// Discover lists the Discovered Instances: those that answer an mDNS
// browse, then those at the static link addresses, without this Instance
// and each once. Those already paired are marked with their Link, and those
// that cannot be paired with their problem.
func (m *Manager) Discover(ctx context.Context) ([]Discovered, error) {
	var (
		wg      sync.WaitGroup
		browsed []mdns.Entry
		berr    error
		static  = make([]Discovered, len(m.cfg.StaticAddrs))
	)
	o := m.cfg.MDNS
	o.Log = m.cfg.Log
	wg.Go(func() { browsed, berr = mdns.Browse(ctx, ServiceType, o) })
	for i, addr := range m.cfg.StaticAddrs {
		wg.Go(func() { static[i] = m.identifyStatic(ctx, addr) })
	}
	wg.Wait()
	if berr != nil && len(m.cfg.StaticAddrs) == 0 {
		return nil, fmt.Errorf("browsing the LAN over mDNS: %w", berr)
	}
	if berr != nil {
		m.cfg.Log.Warn("cannot browse the LAN over mDNS; listing the static link addresses only", "err", berr)
	}
	var out []Discovered
	for _, e := range browsed {
		if d, ok := fromEntry(e); ok {
			out = append(out, d)
		}
	}
	out = append(out, static...)
	links := m.Links()
	seen := map[string]bool{}
	out = slices.DeleteFunc(out, func(d Discovered) bool {
		key := d.InstanceID + " " + d.Fingerprint
		if d.InstanceID == "" {
			key = d.Address
		}
		if d.InstanceID == m.cfg.ID || seen[key] {
			return true
		}
		seen[key] = true
		return false
	})
	for i := range out {
		m.mark(&out[i], links)
	}
	return out, nil
}

// mark marks d with its Link and its problem.
func (m *Manager) mark(d *Discovered, links []Link) {
	if d.Problem != "" {
		return
	}
	for _, l := range links {
		switch {
		case l.Fingerprint == d.Fingerprint:
			d.Link = l.Name
		case l.InstanceID == d.InstanceID:
			d.Problem = fmt.Sprintf("its Instance CA %s is not the one pinned for link %q; if it was rotated, unpair and pair again", d.Fingerprint, l.Name)
		}
	}
	if _, err := Negotiate(m.cfg.Versions, Versions{d.MinVersion, d.Version}, fmt.Sprintf("Instance %q", d.Instance)); err != nil {
		d.Problem = err.Error()
	}
}

// fromEntry is the Discovered Instance an mDNS entry advertises, if it
// says enough to be one.
func fromEntry(e mdns.Entry) (Discovered, bool) {
	var ip net.IP
	for _, a := range e.Addrs {
		if ip == nil || (ip.To4() == nil && a.To4() != nil) {
			ip = a
		}
	}
	if ip == nil || e.TXT[txtID] == "" {
		return Discovered{}, false
	}
	v, _ := strconv.Atoi(e.TXT[txtVersion])
	minV, _ := strconv.Atoi(e.TXT[txtMinVersion])
	return Discovered{
		Name: e.Instance, Instance: e.TXT[txtName], InstanceID: e.TXT[txtID],
		Address: net.JoinHostPort(ip.String(), strconv.Itoa(e.Port)), Fingerprint: e.TXT[txtFingerprint],
		Version: v, MinVersion: minV, Source: SourceMDNS,
	}, true
}

// identifyStatic asks the Instance at a static link address who it is.
func (m *Manager) identifyStatic(ctx context.Context, addr string) Discovered {
	addr = Dialable(addr)
	host, _, _ := net.SplitHostPort(addr)
	id, err := m.Identify(ctx, addr)
	if err != nil {
		return Discovered{Name: addr, Address: addr, Source: SourceStatic, Problem: err.Error()}
	}
	return Discovered{
		Name: id.Name + "@" + host, Instance: id.Name, InstanceID: id.ID, Address: addr, Fingerprint: id.Fingerprint,
		Version: id.Version, MinVersion: id.MinVersion, Source: SourceStatic,
	}
}
