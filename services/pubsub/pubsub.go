// Package pubsub emulates Cloud Pub/Sub (SRS 5.5): topics, subscriptions,
// publish, pull, streaming pull and push delivery, ordering, dead-letter
// topics, filters, exactly-once delivery, snapshots, seek and schemas.
//
// The gRPC services google.pubsub.v1.{Publisher,Subscriber,SchemaService}
// are served on the gateway and on the dedicated Pub/Sub port (default 8085,
// for PUBSUB_EMULATOR_HOST); the REST v1 surface is mounted on the gateway
// under /pubsub/ and served on the dedicated port too.
package pubsub

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/gateway"
)

// Service is the Pub/Sub service module.
type Service struct {
	env *emu.Env

	mu      sync.Mutex
	topics  map[string]*topic
	subs    map[string]*sub
	snaps   map[string]*snapshot
	schemas map[string]*schemaHistory
	msgs    map[uint64]*message
	seq     uint64 // last assigned message sequence (= message ID)
	leaseN  uint64

	dlqMu   sync.Mutex
	dlqQ    []dlqItem
	dlqWake chan struct{}

	loaded   atomic.Bool
	started  atomic.Bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	grpcSrv  *grpc.Server
	httpSrv  *http.Server
	push     *http.Client
	stopping chan struct{}
}

var (
	_ emu.Service   = (*Service)(nil)
	_ emu.Resetter  = (*Service)(nil)
	_ emu.Seeder    = (*Service)(nil)
	_ emu.EnvVarer  = (*Service)(nil)
	_ emu.Publisher = (*Service)(nil)

	_ emu.ClockObserver = (*Service)(nil)
)

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{
		env:      env,
		dlqWake:  make(chan struct{}, 1),
		stopping: make(chan struct{}),
		push:     &http.Client{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.resetMemory()
	return s
}

func (s *Service) Name() string { return "pubsub" }

// resetMemory clears in-memory state; callers hold s.mu.
func (s *Service) resetMemory() {
	for _, sb := range s.subs {
		sb.deleted = true
		s.stopPush(sb)
		sb.signal()
	}
	s.topics = map[string]*topic{}
	s.subs = map[string]*sub{}
	s.snaps = map[string]*snapshot{}
	s.schemas = map[string]*schemaHistory{}
	s.msgs = map[uint64]*message{}
	s.seq = 0
}

// Register loads persisted state and mounts the gRPC and REST APIs on the
// gateway (FR-CORE-040).
func (s *Service) Register(r emu.Router) error {
	if err := s.load(); err != nil {
		return err
	}
	s.loaded.Store(true)
	s.registerGRPC(r.GRPC())
	r.Mount("pubsub", []string{"pubsub.googleapis.com"}, s.restHandler())
	return nil
}

// registerGRPC registers the Pub/Sub services on g. google.iam.v1.IAMPolicy
// is shared by every API, so on the gateway it is only registered when no
// other service has registered it first.
func (s *Service) registerGRPC(g *grpc.Server) {
	pubsubpb.RegisterPublisherServer(g, &publisherServer{s: s})
	pubsubpb.RegisterSubscriberServer(g, &subscriberServer{s: s})
	pubsubpb.RegisterSchemaServiceServer(g, &schemaServer{s: s})
	if _, taken := g.GetServiceInfo()[iampb.IAMPolicy_ServiceDesc.ServiceName]; !taken {
		iampb.RegisterIAMPolicyServer(g, &iamServer{s: s})
	}
}

// Start opens the dedicated Pub/Sub port (FR-CORE-041), which serves gRPC
// (for PUBSUB_EMULATOR_HOST) and REST, and starts background delivery.
func (s *Service) Start(ctx context.Context) error {
	var opts []grpc.ServerOption
	if s.env.GRPCOptions != nil {
		opts = s.env.GRPCOptions("pubsub")
	}
	s.grpcSrv = grpc.NewServer(opts...)
	s.registerGRPC(s.grpcSrv)
	reflection.Register(s.grpcSrv)

	l, err := s.env.Listen("pubsub")
	if err != nil {
		return err
	}
	rest := s.restHandler()
	if s.env.Middleware != nil {
		rest = s.env.Middleware("pubsub", rest)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && len(r.Header.Get("Content-Type")) >= 16 && r.Header.Get("Content-Type")[:16] == "application/grpc" {
			s.grpcSrv.ServeHTTP(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	})
	s.httpSrv = gateway.NewServer(h, s.env.Log)
	// Server-side HTTP/2 keepalive pings keep idle streaming pulls alive.
	s.httpSrv.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 20 * time.Second}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.httpSrv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			s.env.Log.Error("pubsub listener stopped", "err", err)
		}
	}()

	s.wg.Add(2)
	go s.tickLoop()
	go s.dlqLoop()
	s.started.Store(true)
	s.mu.Lock()
	for _, sb := range s.subs {
		s.syncPush(sb)
	}
	s.mu.Unlock()
	return nil
}

// Stop ends streams, background work and the dedicated listener.
func (s *Service) Stop(ctx context.Context) error {
	select {
	case <-s.stopping:
	default:
		close(s.stopping)
	}
	s.cancel()
	s.mu.Lock()
	for _, sb := range s.subs {
		s.stopPush(sb)
	}
	s.mu.Unlock()
	var err error
	if s.httpSrv != nil {
		err = s.httpSrv.Shutdown(ctx)
	}
	if s.grpcSrv != nil {
		s.grpcSrv.Stop()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return err
}

// Ready reports whether state has been loaded.
func (s *Service) Ready() error {
	if !s.loaded.Load() {
		return errors.New("loading state")
	}
	return nil
}

// Reset drops all in-memory state (FR-CORE-002); the store is wiped by the caller.
func (s *Service) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetMemory()
	s.dlqMu.Lock()
	s.dlqQ = nil
	s.dlqMu.Unlock()
	return nil
}

// EnvVars implements emu.EnvVarer (FR-CORE-004).
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	out := map[string]string{}
	if ep := endpoints["pubsub"]; ep != "" {
		out["PUBSUB_EMULATOR_HOST"] = ep
	}
	if gw != "" {
		out["CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB"] = "http://" + gw + "/pubsub/"
	}
	return out
}

// ClockAdvanced re-applies message retention and snapshot expiry after
// `gcpemu time advance`.
func (s *Service) ClockAdvanced(ctx context.Context) error {
	s.gc()
	return nil
}

// tickLoop expires leases, promotes delayed redeliveries and runs
// retention garbage collection.
func (s *Service) tickLoop() {
	defer s.wg.Done()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	lastGC := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		s.mu.Lock()
		for _, sb := range s.subs {
			s.expire(sb, now)
			s.promote(sb, now)
		}
		s.mu.Unlock()
		if now.Sub(lastGC) >= 5*time.Second {
			lastGC = now
			s.gc()
		}
	}
}

// gc enforces message retention on topics, subscriptions and snapshots.
func (s *Service) gc() {
	now := s.env.Clock.Now()
	var w writes
	s.mu.Lock()
	for _, t := range s.topics {
		ret := t.retention()
		i := 0
		for i < len(t.log) && (ret == 0 || now.Sub(t.log[i].publish) >= ret) {
			s.unref(t.log[i], &w)
			i++
		}
		if i > 0 {
			t.log = append([]*message(nil), t.log[i:]...)
		}
	}
	for _, sb := range s.subs {
		ret := defaultSubRetention
		if d := sb.cfg.GetMessageRetentionDuration(); d != nil {
			ret = d.AsDuration()
		}
		for seq, e := range sb.entries {
			if now.Sub(e.m.publish) < ret || e.leased() || e.dlq {
				continue
			}
			s.dequeue(sb, e)
			delete(sb.entries, seq)
			w.del(nsBacklog, backlogKey(sb.name, seq))
			s.unref(e.m, &w)
		}
	}
	for name, sn := range s.snaps {
		if exp := sn.cfg.GetExpireTime(); exp != nil && now.After(exp.AsTime()) {
			s.dropSnapshot(name, &w)
		}
	}
	s.mu.Unlock()
	if err := s.commit(&w); err != nil {
		s.env.Log.Warn("pubsub: retention gc", "err", err)
	}
}
