package sql

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Backup expiry (FR-SQL-008), on the emulator clock: a periodic sweep and
// ClockAdvanced delete, with their dumps, the Final backups whose
// expiryTime is not after the clock. Like Cloud SQL it records no
// operation. A backup still being taken, or held by a restore about to
// read it, is kept until a later sweep.

// expirySweepInterval is how often expired backups are deleted without a
// clock advance.
const expirySweepInterval = time.Second

// ClockAdvanced deletes the backups that expired before returning
// (`gcpemu time advance`).
func (s *Service) ClockAdvanced(ctx context.Context) error {
	s.expireBackups()
	return nil
}

// expiryLoop sweeps every expirySweepInterval until ctx is done.
func (s *Service) expiryLoop(ctx context.Context) {
	t := time.NewTicker(expirySweepInterval)
	defer t.Stop()
	for {
		s.expireBackups()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// expiry is b's expiryTime, "" for none. Only a Final backup expires, as
// in Cloud SQL; an on-demand backup's stored expiryTime, as an older state
// snapshot can hold, is ignored.
func (b *backupRecord) expiry() string {
	if b.Run.Type != "FINAL" {
		return ""
	}
	return b.ExpiryTime
}

// expired reports whether b is due for deletion at now.
func (b *backupRecord) expired(now time.Time) bool {
	if b.expiry() == "" || b.Run.Status == "RUNNING" || b.Run.Status == "ENQUEUED" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, b.expiry())
	return err == nil && !t.After(now)
}

// expireBackups deletes the backups that are due. Each is checked again
// in the transaction that deletes it, so an update or hold since the scan
// keeps it.
func (s *Service) expireBackups() {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	now := s.env.Clock.Now()
	var keys []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsBackups, "", func(k string, b *backupRecord) {
			if b.expired(now) {
				keys = append(keys, k)
			}
		})
		return nil
	})
	for _, k := range keys {
		var file string
		err := s.env.Store.Update(func(tx store.Tx) error {
			var b backupRecord
			if store.GetJSON(tx, nsBackups, k, &b) != nil || !b.expired(now) || s.held(k) {
				return nil
			}
			file = b.File
			return tx.Delete(nsBackups, k)
		})
		if err != nil {
			s.env.Log.Warn("sql: deleting an expired backup failed", "backup", k, "err", err)
			continue
		}
		if file != "" {
			_ = os.Remove(file)
		}
	}
}

// held reports whether a restore holds the backup stored at key.
func (s *Service) held(key string) bool {
	s.inUseMu.Lock()
	defer s.inUseMu.Unlock()
	return s.inUse[key] > 0
}

// holdBackup keeps b from expiry until release is called; ok is false
// (and nothing is held) when b no longer exists.
func (s *Service) holdBackup(project string, b *backupRecord) (release func(), ok bool) {
	key := b.key(project)
	s.inUseMu.Lock()
	s.inUse[key]++
	s.inUseMu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			s.inUseMu.Lock()
			if s.inUse[key]--; s.inUse[key] <= 0 {
				delete(s.inUse, key)
			}
			s.inUseMu.Unlock()
		})
	}
	// A sweep checks holds in the transaction that deletes; checking in
	// a write transaction too orders the two, so a backup still stored
	// now stays until release.
	_ = s.env.Store.Update(func(tx store.Tx) error {
		_, ok = tx.Get(nsBackups, key)
		return nil
	})
	if !ok {
		release()
	}
	return release, ok
}
