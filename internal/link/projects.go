package link

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// Shared and Delegated Projects (ADR 0027): a Linked Instance shares a
// Project, and only then can the front Instance delegate it, after which
// the front Instance forwards every request naming it over the link.

// Share offers the Project id to the link with, or to every link when with
// is "". Sharing alone forwards nothing.
func (m *Manager) Share(id, with string) error {
	if !project.ValidID(id) {
		return fmt.Errorf("invalid Project ID %q", id)
	}
	return m.cfg.Store.update(func(st *stored) error {
		if with != "" && !slices.ContainsFunc(st.Links, func(l Link) bool { return l.Name == with }) {
			return fmt.Errorf("no link named %q; `gcpemu link list` lists them", with)
		}
		if d, ok := delegation(st.Delegated, id); ok {
			return fmt.Errorf("Project %q is delegated to link %q, so this Instance cannot share it", id, d.Link)
		}
		sh := Share{Project: id, With: with}
		if !slices.Contains(st.Shared, sh) {
			st.Shared = append(st.Shared, sh)
		}
		return nil
	})
}

// Unshare stops offering the Project id to the link with, or to every link
// when with is "". Requests a front Instance still forwards for it are
// refused.
func (m *Manager) Unshare(id, with string) error {
	return m.cfg.Store.update(func(st *stored) error {
		n := len(st.Shared)
		if with != "" && slices.Contains(st.Shared, Share{Project: id}) {
			return fmt.Errorf("Project %q is shared with every link; unshare it without --with", id)
		}
		st.Shared = slices.DeleteFunc(st.Shared, func(sh Share) bool { return sh.Project == id && (with == "" || sh.With == with) })
		if len(st.Shared) == n {
			if with != "" {
				return fmt.Errorf("Project %q is not shared with link %q", id, with)
			}
			return fmt.Errorf("Project %q is not shared", id)
		}
		return nil
	})
}

// Shares returns the Projects this Instance shares, by Project.
func (m *Manager) Shares() []Share { return m.cfg.Store.Shares() }

// Shared reports whether this Instance shares the Project ref, an ID or
// number, with the link name.
func (m *Manager) Shared(name, ref string) bool {
	return slices.ContainsFunc(m.SharedWith(name), func(id string) bool { return names(id, ref) })
}

// SharedWith returns the Projects this Instance shares with the link name.
func (m *Manager) SharedWith(name string) []string {
	out := []string{}
	for _, sh := range m.Shares() {
		if (sh.With == "" || sh.With == name) && !slices.Contains(out, sh.Project) {
			out = append(out, sh.Project)
		}
	}
	return out
}

// serveShared answers a paired Instance with the Projects shared with it.
func (m *Manager) serveShared(w http.ResponseWriter, r *http.Request) {
	p, _, ok := m.admit(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": m.SharedWith(p.Name)})
}

// Delegate delegates the Project id to the link to, which must share it
// with this Instance, and learns the link's buckets before it returns. It refuses a Project this Instance has, or shares,
// since local state is never shadowed or merged.
func (m *Manager) Delegate(ctx context.Context, id, to string) (Delegation, error) {
	if !project.ValidID(id) {
		return Delegation{}, fmt.Errorf("invalid Project ID %q", id)
	}
	if d, ok := m.delegated(id); ok {
		return Delegation{}, fmt.Errorf("Project %q is already delegated to link %q; undelegate it first", id, d.Link)
	}
	if m.cfg.HasProject != nil && m.cfg.HasProject(id) {
		return Delegation{}, fmt.Errorf("Instance %q already has a Project %q, so it cannot delegate it: local state is never shadowed or merged", m.cfg.Instance, id)
	}
	if slices.ContainsFunc(m.Shares(), func(sh Share) bool { return sh.Project == id }) {
		return Delegation{}, fmt.Errorf("Instance %q shares Project %q, so it cannot delegate it", m.cfg.Instance, id)
	}
	l, err := m.usable(to)
	if err != nil {
		return Delegation{}, err
	}
	var shared struct{ Projects []string }
	if _, err := m.callPaired(ctx, l, http.MethodGet, "/link/v1/shared", &shared); err != nil {
		return Delegation{}, fmt.Errorf("asking link %q which Projects it shares: %w", to, err)
	}
	if !slices.Contains(shared.Projects, id) {
		return Delegation{}, fmt.Errorf("Project %q is not shared with this Instance by link %q; share it there with `gcpemu link share %s`", id, to, id)
	}
	d := Delegation{Project: id, Link: to}
	err = m.cfg.Store.update(func(st *stored) error {
		if _, ok := delegation(st.Delegated, id); ok {
			return fmt.Errorf("Project %q is already delegated", id)
		}
		st.Delegated = append(st.Delegated, d)
		return nil
	})
	if err != nil {
		return Delegation{}, err
	}
	m.cfg.Log.Info("Project delegated", "project", id, "link", to)
	m.watch()
	// The link's buckets are routed from the moment Delegate returns,
	// not from when its watch first hears of them.
	m.RefreshBuckets(ctx, to)
	return d, nil
}

// Undelegate stops delegating the Project id. It holds no state on this
// Instance, so it does not exist here until something uses it again.
func (m *Manager) Undelegate(id string) error {
	defer m.watch()
	return m.cfg.Store.update(func(st *stored) error {
		n := len(st.Delegated)
		st.Delegated = slices.DeleteFunc(st.Delegated, func(d Delegation) bool { return d.Project == id })
		if len(st.Delegated) == n {
			return fmt.Errorf("Project %q is not delegated", id)
		}
		return nil
	})
}

// Delegations returns the Delegated Projects, by Project.
func (m *Manager) Delegations() []Delegation { return m.cfg.Store.Delegations() }

// DelegatedTo returns the link the Project ref, an ID or number, is
// delegated to.
func (m *Manager) DelegatedTo(ref string) (string, bool) {
	for _, d := range m.Delegations() {
		if names(d.Project, ref) {
			return d.Link, true
		}
	}
	return "", false
}

// DelegatedLinks returns the links any Project is delegated to, by name.
func (m *Manager) DelegatedLinks() []string {
	var out []string
	for _, d := range m.Delegations() {
		if !slices.Contains(out, d.Link) {
			out = append(out, d.Link)
		}
	}
	slices.Sort(out)
	return out
}

// Delegating reports whether this Instance delegates any Project.
func (m *Manager) Delegating() bool { return len(m.Delegations()) > 0 }

// names reports whether ref, a Project ID or number, names the Project id.
func names(id, ref string) bool {
	return id == ref || ref != "" && strings.Trim(ref, "0123456789") == "" && project.NumberString(id) == ref
}

func (m *Manager) delegated(id string) (Delegation, bool) {
	return delegation(m.Delegations(), id)
}

// delegation returns the Delegation of the Project id in ds.
func delegation(ds []Delegation, id string) (Delegation, bool) {
	for _, d := range ds {
		if d.Project == id {
			return d, true
		}
	}
	return Delegation{}, false
}
