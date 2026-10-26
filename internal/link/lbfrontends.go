package link

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// Load balancing frontends (ADR 0027, #715): a forwarding rule's
// IPAddress is its Declared address for every caller, which only the
// Linked Instance's machine reaches, so a caller on the front Instance's
// machine finds a Delegated Project's --lb-bind addresses in the front
// Instance's `gcpemu env` instead. The front Instance asks each Linked
// Instance it delegates to for the Load balancing frontends of the
// Projects shared with it, which it answers as it would a LAN caller on
// the --link-bind address the request arrived at.

// lbFrontendsWait is the longest the front Instance waits for a Linked
// Instance's Load balancing frontends, so that `gcpemu env` never hangs on one.
const lbFrontendsWait = 5 * time.Second

// LBFrontend is a Load balancing frontend, a forwarding rule's or a GKE
// LoadBalancer Service port's, as a Linked Instance tells a LAN caller it.
type LBFrontend struct {
	// Name is its Service endpoint name, by which `gcpemu env` names its
	// variables (ADR 0013).
	Name    string `json:"name"`
	Project string `json:"project"`
	// What names it in messages.
	What string `json:"what"`
	// Address is the --lb-bind address:port to tell the caller, "" when
	// there is none, and Error then says why.
	Address string `json:"address,omitempty"`
	Error   string `json:"error,omitempty"`
}

// LinkFrontends is what a Linked Instance said of its Load balancing
// frontends.
type LinkFrontends struct {
	Link      string       `json:"link"`
	Frontends []LBFrontend `json:"frontends"`
	// LBBind is set when the Linked Instance has --lb-bind.
	LBBind bool `json:"lbBind"`
	// Error says why the link could not be asked, which leaves out its
	// Load balancing frontends.
	Error string `json:"error,omitempty"`
}

// serveLBFrontends answers a paired Instance with the Load balancing
// frontends of the Projects shared with it, as told to a LAN caller on the
// address the request arrived at.
func (m *Manager) serveLBFrontends(w http.ResponseWriter, r *http.Request) {
	p, _, ok := m.admit(w, r)
	if !ok {
		return
	}
	out := []LBFrontend{}
	if m.cfg.LBFrontends != nil {
		for _, e := range m.cfg.LBFrontends(arrivedAt(r)) {
			if m.Shared(p.Name, e.Project) {
				out = append(out, e)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"frontends": out, "lbBind": m.cfg.LBBind != nil && m.cfg.LBBind()})
}

// arrivedAt returns the IP address r arrived at.
func arrivedAt(r *http.Request) string {
	a, _ := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if a == nil {
		return ""
	}
	if v4 := a.IP.To4(); v4 != nil {
		return v4.String()
	}
	return a.IP.String()
}

// lbReply is a Linked Instance's answer to askLBFrontends.
type lbReply struct {
	Frontends []LBFrontend
	LBBind    bool
}

// askLBFrontends asks the link name for its Load balancing frontends.
func (m *Manager) askLBFrontends(ctx context.Context, name string) (lbReply, error) {
	var reply lbReply
	l, err := m.usable(name)
	if err != nil {
		return reply, err
	}
	_, err = m.callPaired(ctx, l, http.MethodGet, "/link/v1/lb-frontends", &reply)
	return reply, err
}

// LinkedLBFrontends asks each link this Instance delegates a Project to,
// at once, for the Load balancing frontends of the Projects delegated to
// it, by link. A link it could not ask has an Error and no Load balancing frontends.
func (m *Manager) LinkedLBFrontends(ctx context.Context) []LinkFrontends {
	ctx, cancel := context.WithTimeout(ctx, lbFrontendsWait)
	defer cancel()
	links := m.DelegatedLinks()
	out := make([]LinkFrontends, len(links))
	var wg sync.WaitGroup
	for i, name := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = LinkFrontends{Link: name, Frontends: []LBFrontend{}}
			reply, err := m.askLBFrontends(ctx, name)
			if err != nil {
				m.cfg.Log.Debug("could not ask a Linked Instance for its Load balancing frontends", "link", name, "err", err)
				out[i].Error = err.Error()
				return
			}
			out[i].LBBind = reply.LBBind
			// A Linked Instance speaks only for the Projects delegated to
			// it.
			for _, e := range reply.Frontends {
				if to, ok := m.DelegatedTo(e.Project); ok && to == name {
					out[i].Frontends = append(out[i].Frontends, e)
				}
			}
		}()
	}
	wg.Wait()
	return out
}
