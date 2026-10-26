package link

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// Load balancing endpoints (ADR 0027, #715): a forwarding rule's
// IPAddress is its Declared address for every caller, which only the
// Linked Instance's machine reaches, so a caller on the front Instance's
// machine finds a Delegated Project's --lb-bind addresses in the front
// Instance's `gcpemu env` instead. The front Instance asks each Linked
// Instance it delegates to for the load balancing endpoints of the
// Projects shared with it, which it answers as it would a LAN caller on
// the --link-bind address the request arrived at.

// lbEndpointsWait is the longest the front Instance waits for a Linked
// Instance's endpoints, so that `gcpemu env` never hangs on one.
const lbEndpointsWait = 5 * time.Second

// LBEndpoint is a load balancing endpoint, a forwarding rule's or a GKE
// LoadBalancer Service port's, as a Linked Instance tells a LAN caller it.
type LBEndpoint struct {
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

// LinkedLB is what a Linked Instance said of its load balancing
// endpoints.
type LinkedLB struct {
	Link      string       `json:"link"`
	Endpoints []LBEndpoint `json:"endpoints"`
	// LBBind is set when the Linked Instance has --lb-bind.
	LBBind bool `json:"lbBind"`
	// Error says why the link could not be asked, which leaves out its
	// endpoints.
	Error string `json:"error,omitempty"`
}

// serveLBEndpoints answers a paired Instance with the load balancing
// endpoints of the Projects shared with it, as told to a LAN caller on the
// address the request arrived at.
func (m *Manager) serveLBEndpoints(w http.ResponseWriter, r *http.Request) {
	p, _, ok := m.admit(w, r)
	if !ok {
		return
	}
	out := []LBEndpoint{}
	if m.cfg.LBEndpoints != nil {
		for _, e := range m.cfg.LBEndpoints(arrivedAt(r)) {
			if m.Shared(p.Name, e.Project) {
				out = append(out, e)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out, "lbBind": m.cfg.LBBind})
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

// lbReply is a Linked Instance's answer to askLBEndpoints.
type lbReply struct {
	Endpoints []LBEndpoint
	LBBind    bool
}

// askLBEndpoints asks the link name for its load balancing endpoints.
func (m *Manager) askLBEndpoints(ctx context.Context, name string) (lbReply, error) {
	var reply lbReply
	l, err := m.usable(name)
	if err != nil {
		return reply, err
	}
	_, err = m.callPaired(ctx, l, http.MethodGet, "/link/v1/lb-endpoints", &reply)
	return reply, err
}

// LinkedLBEndpoints asks each link this Instance delegates a Project to,
// at once, for the load balancing endpoints of the Projects delegated to
// it, by link. A link it could not ask has an Error and no endpoints.
func (m *Manager) LinkedLBEndpoints(ctx context.Context) []LinkedLB {
	ctx, cancel := context.WithTimeout(ctx, lbEndpointsWait)
	defer cancel()
	links := m.DelegatedLinks()
	out := make([]LinkedLB, len(links))
	var wg sync.WaitGroup
	for i, name := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = LinkedLB{Link: name, Endpoints: []LBEndpoint{}}
			reply, err := m.askLBEndpoints(ctx, name)
			if err != nil {
				m.cfg.Log.Debug("could not ask a Linked Instance for its load balancing endpoints", "link", name, "err", err)
				out[i].Error = err.Error()
				return
			}
			out[i].LBBind = reply.LBBind
			// A Linked Instance speaks only for the Projects delegated to
			// it.
			for _, e := range reply.Endpoints {
				if to, ok := m.DelegatedTo(e.Project); ok && to == name {
					out[i].Endpoints = append(out[i].Endpoints, e)
				}
			}
		}()
	}
	wg.Wait()
	return out
}
