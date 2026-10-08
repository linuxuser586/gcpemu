package sql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Operations (FR-CORE-023): sqladmin has its own Operation resource
// (sql#operation) with status PENDING → RUNNING → DONE and an error list.
// Data-plane work for one instance is serialized by the instance lock.

// lroLatency is the configured minimum operation duration ("sql", or
// "sqladmin" as an alias).
func (s *Service) lroLatency() time.Duration {
	if _, ok := s.env.Config.LROLatency["sql"]; ok {
		return s.env.Config.LRO("sql")
	}
	return s.env.Config.LRO("sqladmin")
}

// newOp returns a PENDING operation on an instance.
func (s *Service) newOp(ctx context.Context, project, instance, opType string) *sqladmin.Operation {
	h := s.env.IDs.Hex(16)
	name := fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	user := emu.PrincipalFrom(ctx).Email()
	if user == "" {
		user = "cloudsql@system"
	}
	return &sqladmin.Operation{
		Kind:          "sql#operation",
		Name:          name,
		OperationType: opType,
		Status:        "PENDING",
		User:          user,
		InsertTime:    s.now(),
		TargetId:      instance,
		TargetProject: project,
		TargetLink:    instanceLink(project, instance),
		SelfLink:      opLink(project, name),
	}
}

func (s *Service) saveOp(op *sqladmin.Operation) {
	_ = s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsOps, op.TargetProject+"/"+op.Name, op)
	})
}

// runOp records op and runs fn under the instance lock. With async=false
// and instant LRO latency the work is done before the response, which is
// returned DONE; otherwise op is returned PENDING and completes in the
// background.
func (s *Service) runOp(op *sqladmin.Operation, async bool, fn func(ctx context.Context) error) *sqladmin.Operation {
	s.saveOp(op)
	latency := s.lroLatency()
	exec := func(ctx context.Context) *sqladmin.Operation {
		mu := s.lock(op.TargetProject, op.TargetId)
		defer mu.Unlock()
		start := time.Now()
		op.Status, op.StartTime = "RUNNING", s.now()
		s.saveOp(op)
		err := fn(ctx)
		if d := latency - time.Since(start); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
			}
		}
		s.finishOp(op, err)
		return op
	}
	if !async && latency == 0 {
		return exec(s.bgCtx)
	}
	cp := *op
	s.goBackground(func(ctx context.Context) { exec(ctx) })
	return &cp
}

// finishOp marks op DONE with err (if any).
func (s *Service) finishOp(op *sqladmin.Operation, err error) {
	op.Status, op.EndTime = "DONE", s.now()
	if op.StartTime == "" {
		op.StartTime = op.EndTime
	}
	if err != nil {
		code, msg := "INTERNAL_ERROR", err.Error()
		var oe *opError
		var ae *apierr.Error
		switch {
		case errors.As(err, &oe):
			code, msg = oe.Code, oe.Message
		case errors.As(err, &ae):
			code, msg = apierr.CodeName(ae.Code), ae.Message
		}
		op.Error = &sqladmin.OperationErrors{Kind: "sql#operationErrors", Errors: []*sqladmin.OperationError{{
			Kind: "sql#operationError", Code: code, Message: msg,
		}}}
		s.env.Log.Warn("sql operation failed", "operation", op.Name, "type", op.OperationType, "instance", op.TargetId, "err", msg)
	}
	s.saveOp(op)
}

// failInterruptedOps fails operations left unfinished by a shutdown.
func (s *Service) failInterruptedOps() {
	_ = s.env.Store.Update(func(tx store.Tx) error {
		var pending []*sqladmin.Operation
		scanJSON(tx, nsOps, "", func(_ string, op *sqladmin.Operation) {
			if op.Status != "DONE" {
				pending = append(pending, op)
			}
		})
		for _, op := range pending {
			op.Status, op.EndTime = "DONE", s.now()
			op.Error = &sqladmin.OperationErrors{Kind: "sql#operationErrors", Errors: []*sqladmin.OperationError{{
				Kind: "sql#operationError", Code: "INTERNAL_ERROR", Message: "The operation was interrupted by an emulator restart.",
			}}}
			if err := store.PutJSON(tx, nsOps, op.TargetProject+"/"+op.Name, op); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) loadOp(project, name string) (*sqladmin.Operation, bool) {
	var op sqladmin.Operation
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.GetJSON(tx, nsOps, project+"/"+name, &op) == nil
		return nil
	})
	return &op, ok
}

func (s *Service) getOperation(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("operation")
	op, ok := s.loadOp(project, name)
	if !ok {
		apierr.Write(w, errOpNotFound(name))
		return
	}
	if err := s.check(r.Context(), "cloudsql.instances.get", instanceResource(project, op.TargetId)); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, op)
}

// waitOperation blocks until the operation is DONE or about 60 s pass.
func (s *Service) waitOperation(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("operation")
	deadline := time.Now().Add(60 * time.Second)
	for {
		op, ok := s.loadOp(project, name)
		if !ok {
			apierr.Write(w, errOpNotFound(name))
			return
		}
		if op.Status == "DONE" || time.Now().After(deadline) {
			writeJSON(w, r, op)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *Service) cancelOperation(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("operation")
	op, ok := s.loadOp(project, name)
	if !ok {
		apierr.Write(w, errOpNotFound(name))
		return
	}
	if op.Status == "DONE" {
		apierr.Write(w, errInvalid("operation %s has already completed.", name))
		return
	}
	apierr.Write(w, apierr.FailedPrecondition("Operation %s of type %s cannot be cancelled.", name, op.OperationType))
}

func (s *Service) listOperations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	project := r.PathValue("project")
	if err := s.env.EnsureProject(project); err != nil {
		apierr.Write(w, err)
		return
	}
	inst := r.URL.Query().Get("instance")
	res := projectResource(project)
	if inst != "" {
		res = instanceResource(project, inst)
	}
	if err := s.check(ctx, "cloudsql.instances.get", res); err != nil {
		apierr.Write(w, err)
		return
	}
	items := map[string]*sqladmin.Operation{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsOps, project+"/", func(_ string, op *sqladmin.Operation) {
			if inst != "" && op.TargetId != inst {
				return
			}
			t, _ := time.Parse(time.RFC3339Nano, op.InsertTime)
			items[fmt.Sprintf("%019d-%s", math.MaxInt64-t.UnixNano(), op.Name)] = op
		})
		return nil
	})
	page, next, err := paginate(r, sortedKeys(items), items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, &sqladmin.OperationsListResponse{Kind: "sql#operationsList", Items: page, NextPageToken: next})
}

// Operations implements emu.OperationLister.
func (s *Service) Operations() []emu.OperationInfo {
	var out []emu.OperationInfo
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsOps, "", func(_ string, b []byte) bool {
			var op sqladmin.Operation
			if json.Unmarshal(b, &op) != nil {
				return true
			}
			oi := emu.OperationInfo{
				Name: "projects/" + op.TargetProject + "/operations/" + op.Name, Project: op.TargetProject,
				Type: op.OperationType, Target: "projects/" + op.TargetProject + "/instances/" + op.TargetId,
				Status: op.Status, Done: op.Status == "DONE",
				StartTime: emu.ParseTime(op.InsertTime), EndTime: emu.ParseTime(op.EndTime), Operation: b,
			}
			if op.Error != nil && len(op.Error.Errors) > 0 {
				var msgs []string
				for _, e := range op.Error.Errors {
					msgs = append(msgs, e.Message)
				}
				oi.Error = &emu.OperationError{Code: op.Error.Errors[0].Code, Message: strings.Join(msgs, "; ")}
			}
			out = append(out, oi)
			return true
		})
		return nil
	})
	return out
}
