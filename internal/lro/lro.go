// Package lro implements google.longrunning Operations for services whose
// mutations are long-running in GCP (FR-CORE-023).
//
// A service creates one Manager, calls Register in its own Register (which
// installs a shared google.longrunning.Operations gRPC server on the gateway
// once per gRPC server) and starts operations with Run. Operations are named
// "<parent>/operations/<uuid>" (normally parent is
// "projects/P/locations/L"), persisted in the store namespace "lro/operations"
// and complete after env.Config.LRO(service); a latency of zero completes
// them before Run returns. ServeREST serves the REST mappings for services
// whose clients poll operations over HTTP.
package lro

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Namespace is the store namespace holding operations, keyed by name.
const Namespace = "lro/operations"

// maxWait caps WaitOperation when neither a timeout nor a deadline is given.
const maxWait = 5 * time.Minute

// record is the persisted form of an operation.
type record struct {
	Service string          `json:"service"`
	Op      json.RawMessage `json:"op"` // protojson google.longrunning.Operation
}

// RunFunc performs the operation's work and returns its response message
// (e.g. the created resource, or emptypb.Empty for deletes).
type RunFunc func(ctx context.Context) (proto.Message, error)

// Manager creates and completes the operations of one service.
type Manager struct {
	env     *emu.Env
	service string

	mu      sync.Mutex
	pending map[string]*time.Timer
	server  *Server
	closed  bool
}

// NewManager returns a manager for service.
func NewManager(env *emu.Env, service string) *Manager {
	return &Manager{env: env, service: service, pending: map[string]*time.Timer{}}
}

// Register installs the shared Operations gRPC server on r (once per gRPC
// server) and marks operations left pending by a previous process as
// aborted, since their work cannot be resumed.
func (m *Manager) Register(r emu.Router) {
	m.recover()
	srv := serverFor(r.GRPC(), m.env.Store)
	srv.add(m)
	m.mu.Lock()
	m.server = srv
	m.mu.Unlock()
}

// Close stops pending timers and detaches the manager from its server.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for name, t := range m.pending {
		t.Stop()
		delete(m.pending, name)
	}
	srv := m.server
	m.server = nil
	m.mu.Unlock()
	if srv != nil {
		srv.remove(m)
	}
}

// Latency returns the configured operation latency for the service.
func (m *Manager) Latency() time.Duration { return m.env.Config.LRO(m.service) }

// Run starts an operation under parent with the given metadata. With zero
// latency run executes synchronously and the returned operation is done;
// otherwise run executes after the latency and the operation is returned
// pending. The ctx values (principal) are kept for the deferred run.
func (m *Manager) Run(ctx context.Context, parent string, metadata proto.Message, run RunFunc) (*longrunningpb.Operation, error) {
	op := &longrunningpb.Operation{Name: parent + "/operations/" + m.newID()}
	if metadata != nil {
		a, err := anypb.New(metadata)
		if err != nil {
			return nil, err
		}
		op.Metadata = a
	}
	d := m.Latency()
	if d <= 0 {
		resp, err := run(ctx)
		if err := finish(op, resp, err); err != nil {
			return nil, err
		}
		if err := m.put(op); err != nil {
			return nil, err
		}
		return op, nil
	}
	if err := m.put(op); err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	m.mu.Lock()
	if !m.closed {
		m.pending[op.Name] = time.AfterFunc(d, func() {
			m.mu.Lock()
			_, live := m.pending[op.Name]
			delete(m.pending, op.Name)
			m.mu.Unlock()
			if !live {
				return
			}
			resp, err := run(bg)
			m.complete(op.Name, resp, err)
		})
	}
	m.mu.Unlock()
	return proto.Clone(op).(*longrunningpb.Operation), nil
}

// newID returns a UUID-formatted operation ID.
func (m *Manager) newID() string {
	h := m.env.IDs.Hex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// finish marks op done with resp or err.
func finish(op *longrunningpb.Operation, resp proto.Message, err error) error {
	op.Done = true
	if err != nil {
		op.Result = &longrunningpb.Operation_Error{Error: apierr.From(err).GRPCStatus().Proto()}
		return nil
	}
	if resp == nil {
		resp = &emptypb.Empty{}
	}
	a, aerr := anypb.New(resp)
	if aerr != nil {
		return aerr
	}
	op.Result = &longrunningpb.Operation_Response{Response: a}
	return nil
}

func (m *Manager) put(op *longrunningpb.Operation) error {
	return m.env.Store.Update(func(tx store.Tx) error { return putOp(tx, m.service, op) })
}

func putOp(tx store.Tx, service string, op *longrunningpb.Operation) error {
	b, err := protojson.Marshal(op)
	if err != nil {
		return err
	}
	return store.PutJSON(tx, Namespace, op.Name, record{Service: service, Op: b})
}

func getOp(tx store.Tx, name string) (*longrunningpb.Operation, string, error) {
	var rec record
	if err := store.GetJSON(tx, Namespace, name, &rec); err != nil {
		return nil, "", err
	}
	op := &longrunningpb.Operation{}
	if err := protojson.Unmarshal(rec.Op, op); err != nil {
		return nil, "", err
	}
	return op, rec.Service, nil
}

// complete records the outcome of a pending operation.
func (m *Manager) complete(name string, resp proto.Message, runErr error) {
	err := m.env.Store.Update(func(tx store.Tx) error {
		op, svc, err := getOp(tx, name)
		if err != nil || op.Done {
			return err
		}
		if err := finish(op, resp, runErr); err != nil {
			return err
		}
		return putOp(tx, svc, op)
	})
	if err != nil && err != store.ErrNotFound {
		m.env.Log.Error("lro: complete operation", "service", m.service, "operation", name, "err", err)
	}
}

// cancel stops a pending operation and marks it CANCELLED.
func (m *Manager) cancel(name string) {
	m.mu.Lock()
	if t, ok := m.pending[name]; ok {
		t.Stop()
		delete(m.pending, name)
	}
	m.mu.Unlock()
	m.complete(name, nil, apierr.New(codes.Canceled, "Operation was cancelled."))
}

// recover aborts operations of this service that a previous process left
// pending.
func (m *Manager) recover() {
	_ = m.env.Store.Update(func(tx store.Tx) error {
		var stale []*longrunningpb.Operation
		tx.Scan(Namespace, "", func(_ string, b []byte) bool {
			var rec record
			if json.Unmarshal(b, &rec) != nil || rec.Service != m.service {
				return true
			}
			op := &longrunningpb.Operation{}
			if protojson.Unmarshal(rec.Op, op) == nil && !op.Done {
				stale = append(stale, op)
			}
			return true
		})
		for _, op := range stale {
			_ = finish(op, nil, apierr.Aborted("Operation was interrupted by an emulator restart."))
			if err := putOp(tx, m.service, op); err != nil {
				return err
			}
		}
		return nil
	})
}

// Get returns an operation of this service.
func (m *Manager) Get(name string) (*longrunningpb.Operation, error) {
	var op *longrunningpb.Operation
	var svc string
	err := m.env.Store.View(func(tx store.Tx) error {
		var err error
		op, svc, err = getOp(tx, name)
		return err
	})
	if err == store.ErrNotFound || (err == nil && svc != m.service) {
		return nil, notFound(name)
	}
	return op, err
}

func notFound(name string) error {
	return apierr.NotFound("Operation %q not found.", name)
}

// ---- shared gRPC server ----

// Server implements google.longrunning.Operations over every operation in
// the store; managers attached to it handle cancellation.
type Server struct {
	longrunningpb.UnimplementedOperationsServer
	st store.Store

	mu       sync.Mutex
	managers map[string]*Manager
}

var (
	serversMu sync.Mutex
	servers   = map[*grpc.Server]*Server{}
)

// serverFor returns (registering if needed) the Operations server for g.
func serverFor(g *grpc.Server, st store.Store) *Server {
	serversMu.Lock()
	defer serversMu.Unlock()
	if s, ok := servers[g]; ok {
		return s
	}
	s := &Server{st: st, managers: map[string]*Manager{}}
	servers[g] = s
	if _, taken := g.GetServiceInfo()["google.longrunning.Operations"]; !taken {
		longrunningpb.RegisterOperationsServer(g, s)
	}
	return s
}

func (s *Server) add(m *Manager) {
	s.mu.Lock()
	s.managers[m.service] = m
	s.mu.Unlock()
}

func (s *Server) remove(m *Manager) {
	s.mu.Lock()
	if s.managers[m.service] == m {
		delete(s.managers, m.service)
	}
	empty := len(s.managers) == 0
	s.mu.Unlock()
	if empty {
		serversMu.Lock()
		for g, v := range servers {
			if v == s {
				delete(servers, g)
			}
		}
		serversMu.Unlock()
	}
}

func (s *Server) manager(service string) *Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.managers[service]
}

func (s *Server) get(name string) (*longrunningpb.Operation, string, error) {
	var op *longrunningpb.Operation
	var svc string
	err := s.st.View(func(tx store.Tx) error {
		var err error
		op, svc, err = getOp(tx, name)
		return err
	})
	if err == store.ErrNotFound {
		return nil, "", notFound(name)
	}
	return op, svc, err
}

// GetOperation implements longrunningpb.OperationsServer.
func (s *Server) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	op, _, err := s.get(req.GetName())
	return op, err
}

// ListOperations implements longrunningpb.OperationsServer; req.Name is the
// parent collection owner (e.g. "projects/P/locations/L").
func (s *Server) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	return listOps(s.st, "", req)
}

// DeleteOperation implements longrunningpb.OperationsServer.
func (s *Server) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	return deleteOp(s.st, "", req.GetName())
}

// CancelOperation implements longrunningpb.OperationsServer.
func (s *Server) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	op, svc, err := s.get(req.GetName())
	if err != nil {
		return nil, err
	}
	if !op.Done {
		if m := s.manager(svc); m != nil {
			m.cancel(op.Name)
		}
	}
	return &emptypb.Empty{}, nil
}

// WaitOperation implements longrunningpb.OperationsServer.
func (s *Server) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	return waitOp(ctx, req, func() (*longrunningpb.Operation, error) {
		op, _, err := s.get(req.GetName())
		return op, err
	})
}

func waitOp(ctx context.Context, req *longrunningpb.WaitOperationRequest, get func() (*longrunningpb.Operation, error)) (*longrunningpb.Operation, error) {
	limit := maxWait
	if t := req.GetTimeout(); t != nil && t.AsDuration() > 0 {
		limit = t.AsDuration()
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		op, err := get()
		if err != nil || op.Done {
			return op, err
		}
		select {
		case <-ctx.Done():
			return op, nil
		case <-tick.C:
		}
	}
}

func listOps(st store.Store, service string, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	prefix := strings.TrimSuffix(req.GetName(), "/") + "/operations/"
	if req.GetName() == "" {
		prefix = ""
	}
	offset := 0
	if t := req.GetPageToken(); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		n, perr := strconv.Atoi(string(b))
		if err != nil || perr != nil || n < 0 {
			return nil, apierr.InvalidArgument("Invalid page token.")
		}
		offset = n
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 1000
	}
	resp := &longrunningpb.ListOperationsResponse{}
	err := st.View(func(tx store.Tx) error {
		i := 0
		var ierr error
		tx.Scan(Namespace, prefix, func(_ string, b []byte) bool {
			var rec record
			if ierr = json.Unmarshal(b, &rec); ierr != nil {
				return false
			}
			if service != "" && rec.Service != service {
				return true
			}
			if i >= offset {
				if len(resp.Operations) == size {
					resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(i)))
					return false
				}
				op := &longrunningpb.Operation{}
				if ierr = protojson.Unmarshal(rec.Op, op); ierr != nil {
					return false
				}
				resp.Operations = append(resp.Operations, op)
			}
			i++
			return true
		})
		return ierr
	})
	return resp, err
}

func deleteOp(st store.Store, service, name string) (*emptypb.Empty, error) {
	err := st.Update(func(tx store.Tx) error {
		_, svc, err := getOp(tx, name)
		if err == store.ErrNotFound || (err == nil && service != "" && svc != service) {
			return notFound(name)
		}
		if err != nil {
			return err
		}
		return tx.Delete(Namespace, name)
	})
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
