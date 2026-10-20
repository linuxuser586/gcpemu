package build

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/inflight"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Why a WORKING Build stops early: the cause its context is cancelled with.
var (
	errCancelled = errors.New("build: cancelled")
	errTimeout   = errors.New("build: timed out")
	errShutdown  = errors.New("build: emulator shutting down")
)

// liveBuild is a WORKING Build this process runs.
type liveBuild struct {
	ref  buildRef
	ctx  context.Context
	stop context.CancelCauseFunc
	// done is closed once the Build has ended and its containers and
	// volumes are gone.
	done chan struct{}

	mu sync.Mutex
	// stepStops stops the running steps, by index, for a step timeout.
	stepStops map[int]context.CancelCauseFunc
}

// halted reports whether the Build was stopped by a Reset or shutdown,
// after which it must not write to the store.
func (r *liveBuild) halted() bool {
	c := context.Cause(r.ctx)
	return errors.Is(c, inflight.ErrReset) || errors.Is(c, errShutdown)
}

func (r *liveBuild) setStepStop(i int, stop context.CancelCauseFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stop == nil {
		delete(r.stepStops, i)
	} else {
		r.stepStops[i] = stop
	}
}

func (r *liveBuild) stopStep(i int, cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stop := r.stepStops[i]; stop != nil {
		stop(cause)
	}
}

// now is the Emulator clock's time for the scheduler and the Builds it
// runs.
func (s *Service) now() time.Time { return s.env.Clock.Now() }

// kick asks the scheduler for a pass.
func (s *Service) kick() {
	select {
	case s.kickC <- struct{}{}:
	default:
	}
}

// loop runs scheduler passes when kicked, and every sweepInterval for
// the Emulator clock's own progress.
func (s *Service) loop(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kickC:
		case <-t.C:
		}
		s.schedule(ctx)
	}
}

// schedule expires QUEUED Builds past their queueTtl, starts QUEUED Builds
// while fewer than build.concurrency are WORKING, and times out WORKING
// Builds and steps past their timeout. It waits (bounded by ctx) for the
// Builds it times out to end.
func (s *Service) schedule(ctx context.Context) {
	now := s.now()
	var ended []*liveBuild
	s.mu.Lock()
	if s.paused {
		s.mu.Unlock()
		return
	}
	queue := s.queue[:0]
	for _, ref := range s.queue {
		b, err := s.load(ref)
		if err != nil || b.GetStatus() != cloudbuildpb.Build_QUEUED {
			continue
		}
		if ttl := b.GetQueueTtl().AsDuration(); ttl > 0 && !now.Before(b.GetCreateTime().AsTime().Add(ttl)) {
			s.end(ref, outcome{status: cloudbuildpb.Build_EXPIRED, detail: "Build expired before it could start: queueTtl passed."})
			continue
		}
		if len(s.live) < s.env.Config.Build.Concurrency {
			s.begin(ref)
			continue
		}
		queue = append(queue, ref)
	}
	s.queue = queue
	for _, r := range s.live {
		b, err := s.load(r.ref)
		if err != nil || b.GetStatus() != cloudbuildpb.Build_WORKING {
			continue
		}
		if d := b.GetTimeout().AsDuration(); d > 0 && !now.Before(b.GetStartTime().AsTime().Add(d)) {
			r.stop(errTimeout)
			ended = append(ended, r)
			continue
		}
		for i, st := range b.GetSteps() {
			d := st.GetTimeout().AsDuration()
			if st.GetStatus() == cloudbuildpb.Build_WORKING && d > 0 && !now.Before(st.GetTiming().GetStartTime().AsTime().Add(d)) {
				r.stopStep(i, errStepTimeout)
			}
		}
	}
	s.mu.Unlock()
	for _, r := range ended {
		select {
		case <-r.done:
		case <-ctx.Done():
		}
	}
}

// begin makes a QUEUED Build WORKING, publishes that, and runs it.
// Callers hold mu.
func (s *Service) begin(ref buildRef) {
	var b *cloudbuildpb.Build
	err := s.update(ref, func(st *stored) {
		st.build.Status = cloudbuildpb.Build_WORKING
		st.build.StartTime = timestamppb.New(s.now())
		b = proto.Clone(st.build).(*cloudbuildpb.Build)
	})
	if err != nil {
		s.env.Log.Error("build: start", "build", ref.name(), "err", err)
		s.end(ref, outcome{status: cloudbuildpb.Build_INTERNAL_ERROR, detail: "The Build could not start: " + err.Error()})
		return
	}
	s.notify(b)
	ctx, stop := context.WithCancelCause(context.Background())
	r := &liveBuild{ref: ref, ctx: ctx, stop: stop, done: make(chan struct{}), stepStops: map[int]context.CancelCauseFunc{}}
	s.live[ref.key()] = r
	s.work.Enter()
	go s.execute(r, b)
}

// execute runs a WORKING Build's steps, removes its containers and
// volumes, and ends it.
func (s *Service) execute(r *liveBuild, b *cloudbuildpb.Build) {
	defer s.work.Exit()
	defer close(r.done)
	s.logs.line(r.ref, fmt.Sprintf("starting build %q", b.GetId()))
	s.logs.line(r.ref, "")
	stopFlush := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		s.flushLogs(r, b, stopFlush)
	}()
	o, ranToEnd := s.runSteps(r, b)
	s.reach(r.ctx, b, "ended")
	close(stopFlush)
	<-flushed
	s.removeObjects(r.ref)
	if !r.halted() {
		// A cancel or timeout arriving once the Build ran to its own end,
		// as late as during the cleanup above, leaves its outcome (#455).
		if !ranToEnd {
			switch c := context.Cause(r.ctx); {
			case errors.Is(c, errCancelled):
				o = outcome{status: cloudbuildpb.Build_CANCELLED, detail: "Build was cancelled."}
			case errors.Is(c, errTimeout):
				o = outcome{status: cloudbuildpb.Build_TIMEOUT, detail: "Build timed out."}
			}
		}
		s.end(r.ref, o)
	}
	// No Build starts while mu is held, so the daemon is cleared only
	// of the containers this, the last running Build, left.
	s.mu.Lock()
	if len(s.live) == 1 {
		if rt, err := s.env.Containers.Runtime(context.WithoutCancel(r.ctx)); err == nil {
			s.clearDaemon(rt)
		}
	}
	delete(s.live, r.ref.key())
	s.mu.Unlock()
	r.stop(nil)
	s.kick()
}

// reach runs the tests' checkpoint at, if any.
func (s *Service) reach(ctx context.Context, b *cloudbuildpb.Build, at string) {
	if s.checkpoint != nil {
		s.checkpoint(ctx, b.GetId(), at)
	}
}

// outcome is how a Build ended.
type outcome struct {
	status  cloudbuildpb.Build_Status
	detail  string
	failure *cloudbuildpb.Build_FailureInfo
}

// end gives a Build its final status, logs and publishes it, and
// completes its Operation.
//
// The log's last line is written to the log object first: gcloud reads the
// log once more when it sees the Build finished, and stops.
func (s *Service) end(ref buildRef, o outcome) {
	if o.status == cloudbuildpb.Build_SUCCESS {
		s.logs.line(ref, "DONE")
	} else {
		s.logs.line(ref, "ERROR: "+o.status.String()+": "+o.detail)
	}
	if cur, err := s.load(ref); err == nil {
		s.flushLog(cur)
	}
	var b *cloudbuildpb.Build
	var op string
	err := s.update(ref, func(st *stored) {
		st.build.Status, st.build.StatusDetail, st.build.FailureInfo = o.status, o.detail, o.failure
		st.build.FinishTime = timestamppb.New(s.now())
		for _, step := range st.build.Steps {
			if step.Status == cloudbuildpb.Build_WORKING {
				step.Status = cloudbuildpb.Build_CANCELLED
				if o.status == cloudbuildpb.Build_TIMEOUT {
					step.Status = cloudbuildpb.Build_TIMEOUT
				}
			}
		}
		b, op = proto.Clone(st.build).(*cloudbuildpb.Build), st.op
	})
	if err != nil {
		s.env.Log.Error("build: end", "build", ref.name(), "err", err)
		return
	}
	s.notify(b)
	s.ops.Finish(op, &cloudbuildpb.BuildOperationMetadata{Build: b}, b, opError(o.status))
}

// opError is the error a Build's Operation ends with, nil on SUCCESS.
func opError(status cloudbuildpb.Build_Status) error {
	switch status {
	case cloudbuildpb.Build_SUCCESS:
		return nil
	case cloudbuildpb.Build_CANCELLED:
		return apierr.New(codes.Canceled, "Build was cancelled.")
	case cloudbuildpb.Build_TIMEOUT:
		return apierr.New(codes.DeadlineExceeded, "Build timed out.")
	case cloudbuildpb.Build_EXPIRED:
		return apierr.New(codes.DeadlineExceeded, "Build expired before it could start.")
	case cloudbuildpb.Build_INTERNAL_ERROR:
		return apierr.New(codes.Internal, "Build failed due to an internal error.")
	}
	return apierr.New(codes.Unknown, "Build failed; check build logs for details.")
}

// update changes a stored Build.
func (s *Service) update(ref buildRef, fn func(*stored)) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		st, err := get(tx, ref)
		if err != nil {
			return err
		}
		fn(st)
		return put(tx, st)
	})
}

// cancel ends a QUEUED Build at once, and stops a WORKING one, waiting
// (bounded by ctx) until it has ended and its containers are gone.
func (s *Service) cancel(ctx context.Context, ref buildRef) (*cloudbuildpb.Build, error) {
	s.mu.Lock()
	b, err := s.load(ref)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	r := s.live[ref.key()]
	if b.GetStatus() == cloudbuildpb.Build_QUEUED && r == nil {
		s.end(ref, outcome{status: cloudbuildpb.Build_CANCELLED, detail: "Build was cancelled."})
	}
	if r != nil {
		r.stop(errCancelled)
	}
	s.mu.Unlock()
	if r != nil {
		select {
		case <-r.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.load(ref)
}

// cancelOp cancels the Build whose Operation is name.
func (s *Service) cancelOp(name string) {
	var ref *buildRef
	_ = s.env.Store.View(func(tx store.Tx) error {
		return scan(tx, "", func(st *stored) {
			if st.op == name {
				r := refOf(st.build)
				ref = &r
			}
		})
	})
	if ref != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = s.cancel(ctx, *ref)
	}
}

// interrupt ends INTERNAL_ERROR every Build this process is not running
// that is still QUEUED or WORKING in the store: one a previous process
// left, or one restored from a State snapshot.
func (s *Service) interrupt(detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := map[string]bool{}
	for _, r := range s.queue {
		queued[r.key()] = true
	}
	var stale []buildRef
	_ = s.env.Store.View(func(tx store.Tx) error {
		return scan(tx, "", func(st *stored) {
			r := refOf(st.build)
			if !terminal(st.build.GetStatus()) && s.live[r.key()] == nil && !queued[r.key()] {
				stale = append(stale, r)
			}
		})
	})
	for _, r := range stale {
		s.end(r, outcome{status: cloudbuildpb.Build_INTERNAL_ERROR, detail: detail})
	}
}
