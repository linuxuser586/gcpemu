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

// Linked Load balancing frontends (ADR 0027, #715): a front Instance's
// `gcpemu env` names each Delegated Project's forwarding rules and
// LoadBalancer Services at the --lb-bind addresses its Linked Instance
// tells a LAN caller, since their Declared addresses reach only that
// Instance's machine.

// lanFrontendAddrs returns this Instance's Load balancing frontends, as told
// to a LAN caller that arrived at the address lanIP: each at its --lb-bind
// address on lanIP, else the first, or with why it has none.
func (in *Instance) lanFrontendAddrs(lanIP string) []link.LBFrontend {
	svc, ok := in.Env.Lookup("lb")
	if !ok {
		return nil
	}
	lan, ok := svc.(emu.LANFrontendLister)
	if !ok {
		return nil
	}
	ctx := emu.WithLANCaller(context.Background(), lanIP)
	var out []link.LBFrontend
	for _, e := range lan.LANFrontendAddrs() {
		addr, _ := emu.LANServed(ctx, e.Addrs, nil)
		le := link.LBFrontend{Name: e.Name, Project: e.Project, What: e.What, Address: addr}
		if addr == "" {
			le.Error = e.Error
		}
		out = append(out, le)
	}
	return out
}

// linkedFrontends asks the Linked Instances this Instance delegates to
// for their Load balancing frontends, and returns their Service endpoints,
// named to be listed beside this Instance's own, with notes saying which got no address and which
// links could not be asked, and whether any could not.
func (in *Instance) linkedFrontends(ctx context.Context) (eps map[string]string, notes []string, unreachable bool) {
	eps = map[string]string{}
	if in.link == nil || !in.link.Delegating() {
		return eps, nil, false
	}
	for _, l := range in.link.LinkedLBFrontends(ctx) {
		if l.Error != "" {
			notes = append(notes, fmt.Sprintf("link %q is unreachable, so its Load balancing frontends get no variable: %s", l.Link, l.Error))
			unreachable = true
			continue
		}
		if !l.LBBind {
			notes = append(notes, fmt.Sprintf("link %q has no --lb-bind, so none of its Load balancing frontends get a variable, its LoadBalancer Services included", l.Link))
		}
		for _, e := range l.Frontends {
			if e.Address == "" {
				notes = append(notes, fmt.Sprintf("%s on link %q gets no variable: %s", e.What, l.Link, e.Error))
				continue
			}
			eps[lbenv.Linked(e.Name, l.Link)] = e.Address
		}
	}
	if unreachable {
		notes = append(notes, "while a link is unreachable, no GCPEMU_LB_<NAME> is printed, since its forwarding rules may share the name; "+
			"use GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME>. A GCPEMU_GKE_* variable may collide with one of its LoadBalancer Services")
	}
	return eps, notes, unreachable
}

// withoutShort removes from vars each forwarding rule's GCPEMU_LB_<NAME>
// among endpoints: while a Linked Instance is unreachable, its rules
// might share the name, and a short variable is never pointed at one of
// several rules (ADR 0013).
func withoutShort(vars, endpoints map[string]string) {
	for k := range endpoints {
		if short, ok := lbenv.Short(k); ok {
			delete(vars, short)
		}
	}
}

// withheldNotes says which variables lbenv withheld because Load
// balancing frontends of more than one Instance would print them; a collision among this
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
			notes = append(notes, fmt.Sprintf("%s is withheld: more than one Load balancing frontend would print it (%s)", w.Name, strings.Join(names, ", ")))
		}
	}
	return notes
}

// serveLinkedFrontends is GET /_emu/v1/links/lb-frontends: the Load
// balancing frontends of each Linked Instance this Instance delegates to, which
// `gcpemu status` lists.
func (in *Instance) serveLinkedFrontends(w http.ResponseWriter, r *http.Request) {
	links := []link.LinkFrontends{}
	if in.link.Delegating() {
		links = in.link.LinkedLBFrontends(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": links})
}
