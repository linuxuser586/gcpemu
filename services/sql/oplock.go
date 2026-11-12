package sql

import (
	"context"
	"sync"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// The Operation lock: a Cloud SQL instance accepts one Operation at a
// time, and a request for another fails on arrival with Cloud SQL's
// operationInProgress. It is a claim made on the request path, before the
// request writes anything, and is separate from the Cloud SQL instance's
// mutex: the mutex serializes data-plane work, and reads such as the
// Recovery window wait on it behind a running Operation instead of failing.

// hiddenWait is how long a request waits for work on a Cloud SQL instance
// that isn't an Operation a client can see, such as a State snapshot save.
// Tests shorten it.
var hiddenWait = 30 * time.Second

// claimOp returns a new Operation holding the Cloud SQL instance's Operation
// lock, which runOp releases. A caller that then fails before runOp releases it
// with releaseOp.
func (s *Service) claimOp(ctx context.Context, project, instance, opType string) (*sqladmin.Operation, error) {
	op := s.newOp(ctx, project, instance, opType)
	if err := s.acquire(ctx, instKey(project, instance), op.Name); err != nil {
		return nil, err
	}
	return op, nil
}

// claimBackupOp is claimOp for an Operation on b. A Final or Retained backup
// has outlived its Cloud SQL instance, so the Operation takes no lock, even
// if a new Cloud SQL instance has reused the name.
func (s *Service) claimBackupOp(ctx context.Context, project string, b *backupRecord, opType string) (*sqladmin.Operation, error) {
	if b.InstanceDeletionTime != "" {
		return s.newOp(ctx, project, b.Run.Instance, opType), nil
	}
	return s.claimOp(ctx, project, b.Run.Instance, opType)
}

// takeOp gives op the Operation lock of a Cloud SQL instance that was just
// created, so creating one never fails on the lock. The Operation that
// deleted one of that name may not have released it yet; its release then
// finds the lock taken and leaves it.
func (s *Service) takeOp(op *sqladmin.Operation) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.busy[instKey(op.TargetProject, op.TargetId)] = op.Name
}

// acquire takes a Cloud SQL instance's Operation lock for name. Work a client
// cannot see is waited out for up to hiddenWait; a running Operation fails
// the request at once.
func (s *Service) acquire(ctx context.Context, key, name string) error {
	var deadline <-chan time.Time
	for {
		s.opMu.Lock()
		_, busy := s.busy[key]
		hidden := s.hidden[key]
		if !busy && !hidden {
			s.busy[key] = name
		}
		s.opMu.Unlock()
		switch {
		case busy:
			return errInProgress()
		case !hidden:
			return nil
		}
		if deadline == nil {
			deadline = time.After(hiddenWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errRetryShortly()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// tryAcquire takes a Cloud SQL instance's Operation lock for name only if
// neither an Operation nor hidden work holds it.
func (s *Service) tryAcquire(key, name string) bool {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if _, busy := s.busy[key]; busy || s.hidden[key] {
		return false
	}
	s.busy[key] = name
	return true
}

// releaseOp drops op's Operation lock if op holds it.
func (s *Service) releaseOp(op *sqladmin.Operation) {
	key := instKey(op.TargetProject, op.TargetId)
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.busy[key] == op.Name {
		delete(s.busy, key)
	}
}

func errRetryShortly() error {
	return apierr.New(codes.Unavailable, "Other work on the Cloud SQL instance is still running; retry shortly.")
}

// instanceMutex returns the mutex serializing a Cloud SQL instance's
// data-plane work.
func (s *Service) instanceMutex(project, name string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	mu, ok := s.locks[instKey(project, name)]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[instKey(project, name)] = mu
	}
	return mu
}

// hiddenWork is the held mutex of work that isn't an Operation.
type hiddenWork struct {
	s   *Service
	key string
	mu  *sync.Mutex
}

// Unlock ends the work.
func (h *hiddenWork) Unlock() {
	h.s.opMu.Lock()
	delete(h.s.hidden, h.key)
	h.s.opMu.Unlock()
	h.mu.Unlock()
}

// lock holds the Cloud SQL instance's mutex for work that isn't an
// Operation, which requests for an Operation wait out.
func (s *Service) lock(project, name string) *hiddenWork {
	mu := s.instanceMutex(project, name)
	mu.Lock()
	return s.hide(project, name, mu)
}

// lockCtx is lock, giving up when ctx ends. A sync.Mutex offers no wait that
// can end, so it polls.
func (s *Service) lockCtx(ctx context.Context, project, name string) (*hiddenWork, error) {
	mu := s.instanceMutex(project, name)
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return s.hide(project, name, mu), nil
}

// hide marks the work holding mu as hidden.
func (s *Service) hide(project, name string, mu *sync.Mutex) *hiddenWork {
	s.opMu.Lock()
	s.hidden[instKey(project, name)] = true
	s.opMu.Unlock()
	return &hiddenWork{s: s, key: instKey(project, name), mu: mu}
}
