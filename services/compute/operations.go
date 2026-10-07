package compute

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Compute Operations (FR-CORE-023): every mutation returns a
// compute#operation in the global, regional or zonal operations
// collection. With the default instant latency the work is done before the
// response; with --lro-latency compute=D it runs D later and the operation
// is RUNNING until then. Operations can be polled (get), waited on (wait,
// up to two minutes), listed and deleted.

const nsOps = "compute/operations"

// opSpec describes an operation to start.
type opSpec struct {
	project string
	// scope is "global", "regions/R" or "zones/Z".
	scope    string
	opType   string
	target   string // relative path of the target resource
	targetID uint64
}

func (sp opSpec) collection() string {
	if sp.scope == "global" {
		return "projects/" + sp.project + "/global/operations"
	}
	return "projects/" + sp.project + "/" + sp.scope + "/operations"
}

// startOp records an operation and runs fn as its work. Errors from fn are
// reported in the operation's error field, as GCP does for failures after
// the request was accepted.
func (s *Service) startOp(ctx context.Context, sp opSpec, fn func(ctx context.Context) error) (*computev1.Operation, error) {
	now := s.env.Clock.Now()
	name := fmt.Sprintf("operation-%d-%s-%s-%s", now.UnixMilli(), s.env.IDs.Hex(7)[:13], s.env.IDs.Hex(4), s.env.IDs.Hex(4))
	path := sp.collection() + "/" + name
	user := emu.PrincipalFrom(ctx).Email()
	if user == "" {
		user = emu.Principal(s.env.Config.DefaultPrincipal).Email()
	}
	op := &computev1.Operation{
		Kind:          "compute#operation",
		Id:            s.env.IDs.Uint64(),
		Name:          name,
		OperationType: sp.opType,
		TargetLink:    link(sp.target),
		TargetId:      sp.targetID,
		Status:        "RUNNING",
		User:          user,
		Progress:      0,
		InsertTime:    stamp(now),
		StartTime:     stamp(now),
		SelfLink:      link(path),
	}
	switch {
	case strings.HasPrefix(sp.scope, "regions/"):
		op.Region = link("projects/" + sp.project + "/" + sp.scope)
	case strings.HasPrefix(sp.scope, "zones/"):
		op.Zone = link("projects/" + sp.project + "/" + sp.scope)
	}
	d := s.env.Config.LRO("compute")
	if d <= 0 {
		s.finishOp(op, fn(ctx))
		if err := s.putOp(path, op); err != nil {
			return nil, err
		}
		return op, nil
	}
	if err := s.putOp(path, op); err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	s.opMu.Lock()
	if !s.closed {
		s.timers[path] = time.AfterFunc(d, func() {
			s.opMu.Lock()
			_, live := s.timers[path]
			delete(s.timers, path)
			s.opMu.Unlock()
			if !live {
				return
			}
			err := fn(bg)
			_ = s.env.Store.Update(func(tx store.Tx) error {
				cur, ok := get[computev1.Operation](tx, nsOps, path)
				if !ok || cur.Status == "DONE" {
					return nil
				}
				s.finishOp(cur, err)
				return store.PutJSON(tx, nsOps, path, cur)
			})
			s.wakeOps()
		})
	}
	s.opMu.Unlock()
	cp := *op
	return &cp, nil
}

// finishOp marks op DONE, recording err.
func (s *Service) finishOp(op *computev1.Operation, err error) {
	op.Status = "DONE"
	op.Progress = 100
	op.EndTime = stamp(s.env.Clock.Now())
	if err != nil {
		e := apierr.From(err)
		op.Error = &computev1.OperationError{Errors: []*computev1.OperationErrorErrors{{
			Code:    upperSnake(e.LegacyReason),
			Message: e.Message,
		}}}
		op.HttpErrorStatusCode = int64(e.HTTP())
		op.HttpErrorMessage = strings.ToUpper(http.StatusText(e.HTTP()))
	}
}

// upperSnake turns a legacy reason ("resourceInUseByAnotherResource") into
// an operation error code ("RESOURCE_IN_USE_BY_ANOTHER_RESOURCE").
func upperSnake(s string) string {
	if s == "" {
		return "INTERNAL_ERROR"
	}
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

func (s *Service) putOp(path string, op *computev1.Operation) error {
	err := s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsOps, path, op) })
	s.wakeOps()
	return err
}

// wakeOps wakes operation waiters.
func (s *Service) wakeOps() {
	s.opMu.Lock()
	close(s.opWake)
	s.opWake = make(chan struct{})
	s.opMu.Unlock()
}

// recoverOps fails operations a previous process left running.
func (s *Service) recoverOps() {
	_ = s.env.Store.Update(func(tx store.Tx) error {
		for _, op := range list[computev1.Operation](tx, nsOps, "") {
			if op.Status == "DONE" {
				continue
			}
			s.finishOp(op, apierr.Aborted("The operation was aborted because the emulator restarted.").WithLegacy("aborted"))
			if err := store.PutJSON(tx, nsOps, relPath(op.SelfLink), op); err != nil {
				return err
			}
		}
		return nil
	})
}

// opScope maps the collection wildcard of an operations route to a scope.
func opScope(r *http.Request) (scope, perm string, err error) {
	p := r.PathValue("project")
	switch {
	case r.PathValue("region") != "":
		reg := r.PathValue("region")
		return "regions/" + reg, "compute.regionOperations", checkRegion(p, reg)
	case r.PathValue("zone") != "":
		z := r.PathValue("zone")
		return "zones/" + z, "compute.zoneOperations", checkZone(p, z)
	}
	return "global", "compute.globalOperations", nil
}

func opsCollection(project, scope string) string {
	if scope == "global" {
		return "projects/" + project + "/global/operations"
	}
	return "projects/" + project + "/" + scope + "/operations"
}

func (s *Service) getOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.loadOp(r, ".get")
	reply(w, op, err)
}

func (s *Service) loadOp(r *http.Request, verb string) (*computev1.Operation, error) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		return nil, err
	}
	scope, perm, err := opScope(r)
	if err != nil {
		return nil, err
	}
	path := opsCollection(p, scope) + "/" + r.PathValue("operation")
	if err := s.check(r.Context(), perm+verb, path); err != nil {
		return nil, err
	}
	var op *computev1.Operation
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { op, ok = get[computev1.Operation](tx, nsOps, path); return nil })
	if !ok {
		return nil, errNotFound(path)
	}
	return op, nil
}

// waitOperation implements operations.wait: it returns when the operation
// is DONE or after two minutes (or the request deadline), whichever is first.
func (s *Service) waitOperation(w http.ResponseWriter, r *http.Request) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		s.opMu.Lock()
		wake := s.opWake
		s.opMu.Unlock()
		op, err := s.loadOp(r, ".get")
		if err != nil || op.Status == "DONE" {
			reply(w, op, err)
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			reply(w, op, nil)
			return
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) deleteOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.loadOp(r, ".delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	path := relPath(op.SelfLink)
	_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsOps, path) })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) listOperations(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	scope, perm, err := opScope(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	coll := opsCollection(p, scope)
	if err := s.check(r.Context(), perm+".list", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, op := range list[computev1.Operation](tx, nsOps, coll+"/") {
			items = append(items, listItem{key: op.Name, v: op})
		}
		return nil
	})
	writeList(w, r, "compute#operationList", coll, items)
}

// aggregatedOperations implements globalOperations.aggregatedList.
func (s *Service) aggregatedOperations(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.globalOperations.list", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	scoped := map[string][]listItem{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, op := range list[computev1.Operation](tx, nsOps, "projects/"+p+"/") {
			path := relPath(op.SelfLink)
			segs := strings.Split(path, "/")
			sc := "global"
			if len(segs) > 3 && segs[2] != "global" {
				sc = segs[2] + "/" + segs[3]
			}
			scoped[sc] = append(scoped[sc], listItem{key: op.Name, v: op})
		}
		return nil
	})
	writeAggregated(w, r, "compute#operationAggregatedList", "projects/"+p+"/aggregated/operations", "operations", scoped)
}
