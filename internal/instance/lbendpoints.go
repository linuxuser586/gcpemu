package instance

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/lbenv"
	"github.com/linuxuser586/gcpemu/internal/link"
)

// Linked load balancing endpoints (ADR 0027, #715): a front Instance's
// `gcpemu env` names each Delegated Project's forwarding rules and
// LoadBalancer Services at the --lb-bind addresses its Linked Instance
// tells a LAN caller, since their Declared addresses reach only that
// Instance's machine.

// lanEndpoints returns this Instance's load balancing endpoints, as told
// to a LAN caller that arrived at the address lanIP: each at its --lb-bind
// address on lanIP, else the first, or with why it has none.
func (in *Instance) lanEndpoints(lanIP string) []link.LBEndpoint {
	svc, ok := in.Env.Lookup("lb")
	if !ok {
		return nil
	}
	lan, ok := svc.(emu.LANEndpointer)
	if !ok {
		return nil
	}
	ctx := emu.WithLANCaller(context.Background(), lanIP)
	var out []link.LBEndpoint
	for _, e := range lan.LANEndpoints() {
		addr, _ := emu.LANServed(ctx, e.Addrs, nil)
		le := link.LBEndpoint{Name: e.Name, Project: e.Project, What: e.What, Address: addr}
		if addr == "" {
			le.Error = e.Error
		}
		out = append(out, le)
	}
	return out
}

// linkedLB asks the Linked Instances this Instance delegates to for their
// load balancing endpoints, and returns them as endpoints listed beside
// this Instance's own, with notes saying which got no address and which
// links could not be asked.
func (in *Instance) linkedLB(ctx context.Context) (eps map[string]string, notes []string) {
	eps = map[string]string{}
	if in.link == nil || !in.link.Delegating() {
		return eps, nil
	}
	for _, l := range in.link.LinkedLBEndpoints(ctx) {
		if l.Error != "" {
			notes = append(notes, fmt.Sprintf("link %q is unreachable, so its load balancing endpoints get no variable: %s", l.Link, l.Error))
			continue
		}
		if !l.LBBind {
			notes = append(notes, fmt.Sprintf("link %q has no --lb-bind, so none of its load balancing endpoints get a variable, its LoadBalancer Services included", l.Link))
		}
		for _, e := range l.Endpoints {
			if e.Address == "" {
				notes = append(notes, fmt.Sprintf("%s on link %q gets no variable: %s", e.What, l.Link, e.Error))
				continue
			}
			eps[lbenv.Linked(e.Name, l.Link)] = e.Address
		}
	}
	return eps, notes
}

// withheldNotes says which variables lbenv withheld because endpoints of
// more than one Instance would print them; a collision among this
// Instance's own is reported by `gcpemu doctor` instead.
func withheldNotes(ws []lbenv.Withheld) []string {
	var notes []string
	for _, w := range ws {
		across := false
		names := make([]string, len(w.Keys))
		for i, k := range w.Keys {
			name, l := lbenv.Unlinked(k)
			names[i] = name
			if l != "" {
				across = true
				names[i] += fmt.Sprintf(" on link %q", l)
			}
		}
		if across {
			notes = append(notes, fmt.Sprintf("%s is withheld: more than one endpoint would print it (%s)", w.Name, strings.Join(names, ", ")))
		}
	}
	return notes
}

// serveLinkedLB is GET /_emu/v1/links/lb-endpoints: the load balancing
// endpoints of each Linked Instance this Instance delegates to, which
// `gcpemu status` lists.
func (in *Instance) serveLinkedLB(w http.ResponseWriter, r *http.Request) {
	links := []link.LinkedLB{}
	if in.link.Delegating() {
		links = in.link.LinkedLBEndpoints(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": links})
}
