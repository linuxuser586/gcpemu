package gke

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Container operations (FR-CORE-023). The container API has its own
// Operation resource: status PENDING → RUNNING → DONE, an operationType,
// selfLink/targetLink and an error status. Operations complete when their
// data-plane work is done and at least the configured LRO latency
// (--lro-latency container=…) has elapsed.

// workFunc performs an operation's data-plane work.
type workFunc func(ctx context.Context) error

// opKey is the store key of an operation.
func opKey(projectID, name string) string { return projectID + "/" + name }

func (s *Service) newOpName() string {
	return fmt.Sprintf("operation-%d-%s", s.env.Clock.Now().UnixMilli(), s.env.IDs.Hex(4))
}

func (s *Service) now() string { return s.env.Clock.Now().UTC().Format(time.RFC3339Nano) }

// startOp records a RUNNING operation for target and runs work in the
// background. cluster is the cluster key the operation locks (empty for
// none); a second mutating operation on a busy cluster fails like GKE's
// "incompatible operation" error.
func (s *Service) startOp(r ref, typ containerpb.Operation_Type, target string, lock bool, work workFunc) (*containerpb.Operation, error) {
	name := s.newOpName()
	op := &containerpb.Operation{
		Name:          name,
		Zone:          r.Location,
		OperationType: typ,
		Status:        containerpb.Operation_RUNNING,
		SelfLink:      selfLink(projectNumber(r.Project), r.Location, "operations/"+name),
		TargetLink:    target,
		Location:      r.Location,
		StartTime:     s.now(),
		Progress:      &containerpb.OperationProgress{Status: containerpb.Operation_RUNNING},
	}
	if lock {
		s.mu.Lock()
		if cur, ok := s.busy[r.key()]; ok {
			s.mu.Unlock()
			return nil, apierr.FailedPrecondition("Cluster is running incompatible operation %s.", cur).
				WithReason("container.googleapis.com", "CLUSTER_ALREADY_HAS_OPERATION")
		}
		s.busy[r.key()] = name
		s.mu.Unlock()
	}
	if err := s.putOp(r.Project, op); err != nil {
		s.unlock(r, lock, name)
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.opCancel[name] = cancel
	s.mu.Unlock()
	start := time.Now()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		err := work(ctx)
		if d := s.env.Config.LRO("container") - time.Since(start); d > 0 && err == nil {
			select {
			case <-time.After(d):
			case <-ctx.Done():
			}
		}
		if err != nil {
			s.env.Log.Warn("gke operation failed", "operation", name, "type", typ.String(), "target", target, "err", err)
		}
		s.finishOp(r.Project, name, err)
		s.unlock(r, lock, name)
		s.mu.Lock()
		delete(s.opCancel, name)
		s.mu.Unlock()
	}()
	return op, nil
}

func (s *Service) unlock(r ref, lock bool, name string) {
	if !lock {
		return
	}
	s.mu.Lock()
	if s.busy[r.key()] == name {
		delete(s.busy, r.key())
	}
	s.mu.Unlock()
}

func (s *Service) putOp(projectID string, op *containerpb.Operation) error {
	b, err := protojson.Marshal(op)
	if err != nil {
		return err
	}
	return s.env.Store.Update(func(tx store.Tx) error { return tx.Put(nsOperations, opKey(projectID, op.Name), b) })
}

func (s *Service) getOp(projectID, name string) (*containerpb.Operation, error) {
	var b []byte
	_ = s.env.Store.View(func(tx store.Tx) error {
		b, _ = tx.Get(nsOperations, opKey(projectID, name))
		return nil
	})
	if b == nil {
		return nil, apierr.NotFound("Not found: operation %s.", name)
	}
	op := &containerpb.Operation{}
	if err := protojson.Unmarshal(b, op); err != nil {
		return nil, apierr.Internal("corrupt operation %s: %v", name, err)
	}
	return op, nil
}

// finishOp marks an operation DONE, recording err.
func (s *Service) finishOp(projectID, name string, err error) {
	_ = s.env.Store.Update(func(tx store.Tx) error {
		b, ok := tx.Get(nsOperations, opKey(projectID, name))
		if !ok {
			return nil
		}
		op := &containerpb.Operation{}
		if protojson.Unmarshal(b, op) != nil {
			return nil
		}
		op.Status = containerpb.Operation_DONE
		op.EndTime = s.now()
		op.Progress = &containerpb.OperationProgress{Status: containerpb.Operation_DONE}
		if err != nil {
			e := apierr.From(err)
			if s.ctx.Err() != nil || e.Code == codes.Canceled {
				e = apierr.New(codes.Canceled, "Operation was cancelled.")
			}
			op.Error = e.GRPCStatus().Proto()
			op.StatusMessage = op.Error.GetMessage()
			op.Detail = op.Error.GetMessage()
		}
		nb, _ := protojson.Marshal(op)
		return tx.Put(nsOperations, opKey(projectID, name), nb)
	})
}

// listOps returns a project's operations in a location ("-" = all).
func (s *Service) listOps(projectID, location string) []*containerpb.Operation {
	var out []*containerpb.Operation
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsOperations, projectID+"/", func(_ string, b []byte) bool {
			op := &containerpb.Operation{}
			if protojson.Unmarshal(b, op) == nil && (location == "-" || location == "" || op.Location == location) {
				out = append(out, op)
			}
			return true
		})
		return nil
	})
	return out
}

// cancelOp cancels a running operation.
func (s *Service) cancelOp(projectID, name string) error {
	op, err := s.getOp(projectID, name)
	if err != nil {
		return err
	}
	if op.Status == containerpb.Operation_DONE {
		return apierr.FailedPrecondition("Operation %s is already done.", name)
	}
	s.mu.Lock()
	cancel := s.opCancel[name]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// opName extracts the operation ID from "projects/P/locations/L/operations/OP".
func opName(name string) (projectID, location, op string, err error) {
	segs := strings.Split(name, "/")
	if len(segs) != 6 || segs[0] != "projects" || segs[2] != "locations" || segs[4] != "operations" {
		return "", "", "", apierr.InvalidArgument("Invalid operation name %q.", name)
	}
	return segs[1], segs[3], segs[5], nil
}

// abortStaleOps ends operations left running by a previous emulator
// process; their clusters are restored separately.
func (s *Service) abortStaleOps() {
	type stale struct{ project, name string }
	var ops []stale
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsOperations, "", func(k string, b []byte) bool {
			op := &containerpb.Operation{}
			if protojson.Unmarshal(b, op) == nil && op.Status != containerpb.Operation_DONE {
				p, _, _ := strings.Cut(k, "/")
				ops = append(ops, stale{p, op.Name})
			}
			return true
		})
		return nil
	})
	for _, o := range ops {
		s.finishOp(o.project, o.name, apierr.Aborted("The operation was interrupted by an emulator restart."))
	}
}
