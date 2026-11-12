package ar

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Registry cleanup (ADR 0070, #1045): one pass that frees what nothing
// refers to any more. Deleting a version already drops the links only it
// used (deleteManifest); the pass repairs what that cannot see:
//   - blob links that no manifest of their image uses, once older than
//     cleanupGrace on the Emulator clock (a push in progress has linked its
//     layers but not yet put its manifest), in every scope: standard and
//     remote repositories and the hidden Mirror cache scopes;
//   - upload files with no live session, and sessions untouched for longer
//     than cleanupGrace, with their files.
//
// Outside a Deterministic Instance the pass runs at Start and every
// cleanupInterval of real time; in one, only from ClockAdvanced.

// cleanupGrace is how old, on the Emulator clock, an unused blob link or an
// idle upload session must be before the pass frees it.
const cleanupGrace = time.Hour

// cleanupInterval is how often the pass runs outside a Deterministic
// Instance.
const cleanupInterval = time.Minute

// cleanupPass runs the Registry cleanup pass once. Passes run one at a
// time, so ClockAdvanced returns only once the work it asked for is done.
func (s *Service) cleanupPass(ctx context.Context) error {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	if s.blobs == nil {
		return nil
	}
	// Steps that delete versions (cleanup policies, #1039) go before the
	// freeing steps, so that what they leave unused is freed by this pass.
	steps := []func(context.Context) error{
		s.dropOrphanLinks,
		s.dropAbandonedUploads,
	}
	var errs []error
	for _, step := range steps {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, step(ctx))
	}
	return errors.Join(errs...)
}

// dropOrphanLinks drops every blob link that no manifest of its image uses
// and that is older than cleanupGrace, then collects the blobs that lost
// their last reference.
func (s *Service) dropOrphanLinks(context.Context) error {
	now := s.env.Clock.Now()
	var freed []string
	err := s.env.Store.Update(func(tx store.Tx) error {
		freed = freed[:0]
		used := imageBlobs(tx, "")
		var drop []string
		tx.Scan(nsLinks, "", func(k string, b []byte) bool {
			ref, img, d := splitItemKey(k)
			var l linkRec
			if used[imgPrefix(ref, img)][d] || json.Unmarshal(b, &l) != nil || now.Sub(l.Created) <= cleanupGrace {
				return true
			}
			drop = append(drop, k)
			return true
		})
		for _, k := range drop {
			ref, img, d := splitItemKey(k)
			if err := deleteLink(tx, ref, img, d); err != nil {
				return err
			}
			freed = append(freed, d)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.collect(freed)
	return nil
}

// dropAbandonedUploads drops the sessions untouched for longer than
// cleanupGrace, with their files, and then every upload file that has no
// session (one a restart or Reset left behind). A session a request is
// using is busy, not abandoned, and is left alone.
func (s *Service) dropAbandonedUploads(context.Context) error {
	now := s.env.Clock.Now()
	s.mu.Lock()
	sessions := make([]*upload, 0, len(s.uploads))
	for _, u := range s.uploads {
		sessions = append(sessions, u)
	}
	s.mu.Unlock()
	for _, u := range sessions {
		if !u.mu.TryLock() {
			continue
		}
		if now.Sub(u.touched) > cleanupGrace {
			u.dropped = true
			s.dropUpload(u)
		}
		u.mu.Unlock()
	}
	// A session is registered before startUpload releases gc, so under gc
	// every file without a session is abandoned.
	s.blobs.gc.Lock()
	defer s.blobs.gc.Unlock()
	entries, err := os.ReadDir(s.blobs.uploadPath(""))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if _, live := s.uploads[e.Name()]; !live {
			_ = os.RemoveAll(s.blobs.uploadPath(e.Name()))
		}
	}
	return nil
}

// cleanupLoop runs a pass at once and then every cleanupInterval until ctx
// is done.
func (s *Service) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		if err := s.cleanupPass(ctx); err != nil && ctx.Err() == nil {
			s.env.Log.Warn("ar: Registry cleanup pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
