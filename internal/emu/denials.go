package emu

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sync"
	"time"
)

// Denial is a call that the audit IAM enforcement level would deny
// (FR-IAM-004, FR-UI-014): a Principal lacking a permission on a resource,
// or refused it by a deny policy (FR-IAM-010). Calls by the same
// Principal, for the same permission on the same resource, refused by the
// same deny policy or by none, are one Denial.
type Denial struct {
	// ID is stable for the Principal, permission and resource.
	ID         string `json:"id"`
	Principal  string `json:"principal"`
	Permission string `json:"permission"`
	Resource   string `json:"resource"`
	// DenyPolicy names the deny policy that refused the call; it is empty
	// when no allow policy granted the permission.
	DenyPolicy string `json:"denyPolicy,omitempty"`
	// Service and Method are the latest denied call's, as the Request log
	// names them.
	Service string `json:"service,omitempty"`
	Method  string `json:"method,omitempty"`
	// Level is the IAM enforcement level of the latest denied call: audit
	// (allowed) or enforce (failed with PERMISSION_DENIED).
	Level     string    `json:"level"`
	Count     int       `json:"count"`
	FirstTime time.Time `json:"firstTime"`
	LastTime  time.Time `json:"lastTime"`
	// Role is the smallest predefined role granting Permission, and
	// RoleTitle its title; Project is the Project owning Resource, where
	// a binding of Role grants it. They and Granted are worked out when
	// the Denial is read. A deny policy's Denial has no Role, since no
	// grant overrides a deny.
	Role      string `json:"role,omitempty"`
	RoleTitle string `json:"roleTitle,omitempty"`
	Project   string `json:"project,omitempty"`
	// Granted reports that the Principal now holds Permission on Resource,
	// with no deny policy refusing it.
	Granted bool `json:"granted"`

	seq uint64 // of the latest denied call
}

// DenialAdvisor says how to grant a denied permission. The IAM service
// provides it.
type DenialAdvisor interface {
	// AdviseGrant returns the smallest predefined role granting
	// permission, its title, and the Project owning resource ("" when
	// none is known).
	AdviseGrant(ctx context.Context, permission, resource string) (role, title, project string)
}

// denialsKept caps the denial log; the least recently denied go first.
const denialsKept = 1000

// denialLog keeps the Denials an Instance has seen since it started or
// was last Reset.
type denialLog struct {
	mu     sync.Mutex
	byID   map[string]*Denial
	seq    uint64
	onDeny func(Denial)
}

// denialID is the ID of the Denial of permission on resource to p, by
// denyPolicy when it is not empty.
func denialID(p Principal, permission, resource, denyPolicy string) string {
	key := string(p) + "\x00" + permission + "\x00" + resource
	if denyPolicy != "" {
		key += "\x00" + denyPolicy
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

// add records a denied call, refused by denyPolicy when it is not empty.
func (l *denialLog) add(ctx context.Context, p Principal, permission, resource, denyPolicy, level string) {
	id := denialID(p, permission, resource, denyPolicy)
	service, method := callOf(ctx)
	now := time.Now()
	l.mu.Lock()
	if l.byID == nil {
		l.byID = map[string]*Denial{}
	}
	d, ok := l.byID[id]
	if !ok {
		if len(l.byID) >= denialsKept {
			l.evictOldest()
		}
		d = &Denial{ID: id, Principal: string(p), Permission: permission, Resource: resource, DenyPolicy: denyPolicy, FirstTime: now}
		l.byID[id] = d
	}
	l.seq++
	d.Count++
	d.LastTime, d.Level, d.seq = now, level, l.seq
	if service != "" || method != "" {
		d.Service, d.Method = service, method
	}
	out, fn := *d, l.onDeny
	l.mu.Unlock()
	if fn != nil {
		fn(out)
	}
}

func (l *denialLog) evictOldest() {
	var oldest *Denial
	for _, d := range l.byID {
		if oldest == nil || d.seq < oldest.seq {
			oldest = d
		}
	}
	delete(l.byID, oldest.ID)
}

// list returns the Denials, most recently denied first.
func (l *denialLog) list() []Denial {
	l.mu.Lock()
	out := make([]Denial, 0, len(l.byID))
	for _, d := range l.byID {
		out = append(out, *d)
	}
	l.mu.Unlock()
	slices.SortFunc(out, func(a, b Denial) int { return cmp.Compare(b.seq, a.seq) })
	return out
}

func (l *denialLog) clear() {
	l.mu.Lock()
	l.byID = nil
	l.mu.Unlock()
}

// OnDeny sets a function called with every denied call's Denial. Set it
// before the authorizer is used.
func (a *PolicyAuthorizer) OnDeny(fn func(Denial)) { a.denials.onDeny = fn }

// Denials returns the Denials seen since the Instance started or was last
// Reset, most recently denied first, with the advice of the installed
// evaluator and whether each is now granted. A non-empty project keeps
// only that Project's.
func (a *PolicyAuthorizer) Denials(ctx context.Context, project string) []Denial {
	all := a.denials.list()
	out := all[:0]
	advisor, _ := a.eval.(DenialAdvisor)
	denier, _ := a.eval.(DenyEvaluator)
	for _, d := range all {
		if advisor != nil {
			d.Role, d.RoleTitle, d.Project = advisor.AdviseGrant(ctx, d.Permission, d.Resource)
			if d.DenyPolicy != "" {
				d.Role, d.RoleTitle = "", ""
			}
		}
		if project != "" && d.Project != project {
			continue
		}
		if a.eval != nil {
			d.Granted = a.eval.Allowed(ctx, Principal(d.Principal), d.Permission, d.Resource)
		}
		if d.Granted && denier != nil {
			_, denied := denier.Denied(ctx, Principal(d.Principal), d.Permission, d.Resource)
			d.Granted = !denied
		}
		out = append(out, d)
	}
	return out
}

// ClearDenials forgets every Denial.
func (a *PolicyAuthorizer) ClearDenials() { a.denials.clear() }
