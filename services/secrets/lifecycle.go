package secrets

import (
	"context"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Time-driven behaviour, evaluated against the Emulator clock by a
// periodic sweep and whenever the clock is advanced:
//   - a secret past its expire_time is deleted (SECRET_DELETE);
//   - a secret past its next_rotation_time is notified (SECRET_ROTATE),
//     rotated if under managed rotation, and its next_rotation_time
//     advanced by rotation_period (or cleared without one);
//   - a version past its scheduled_destroy_time is destroyed.

type sweepEvent struct {
	ref     secretRef
	event   string
	secret  *secretmanagerpb.Secret
	version *secretmanagerpb.SecretVersion
	managed bool
}

// sweep applies every change that is due at the current clock time.
func (s *Service) sweep(ctx context.Context) {
	now := s.env.Clock.Now()
	var events []sweepEvent
	err := s.env.Store.Update(func(tx store.Tx) error {
		events = events[:0]
		var refs []secretRef
		tx.Scan(nsSecrets, "", func(k string, _ []byte) bool { refs = append(refs, refFromKey(k)); return true })
		for _, ref := range refs {
			rec, err := getSecret(tx, ref)
			if err != nil {
				continue
			}
			if t := rec.pb.GetExpireTime(); t != nil && !t.AsTime().After(now) {
				if err := deleteSecretData(tx, ref); err != nil {
					return err
				}
				events = append(events, sweepEvent{ref: ref, event: "SECRET_DELETE", secret: rec.pb})
				continue
			}
			changed := false
			for _, v := range listVersions(tx, ref) {
				if t := v.pb.GetScheduledDestroyTime(); t != nil && !t.AsTime().After(now) {
					n := versionNumber(v.pb.GetName())
					destroy(v, now)
					if err := putVersion(tx, ref, n, v, now); err != nil {
						return err
					}
					for alias, an := range rec.pb.GetVersionAliases() {
						if an == n {
							delete(rec.pb.VersionAliases, alias)
							changed = true
						}
					}
					events = append(events, sweepEvent{ref: ref, event: "SECRET_VERSION_DESTROY", secret: rec.pb, version: v.pb})
				}
			}
			if rot := rec.pb.GetRotation(); rot.GetNextRotationTime() != nil && !rot.GetNextRotationTime().AsTime().After(now) {
				if p := rot.GetRotationPeriod(); p != nil && p.AsDuration() > 0 {
					next := rot.GetNextRotationTime().AsTime()
					for !next.After(now) {
						next = next.Add(p.AsDuration())
					}
					rot.NextRotationTime = timestamppb.New(next)
				} else {
					rot.NextRotationTime = nil
				}
				changed = true
				events = append(events, sweepEvent{ref: ref, event: "SECRET_ROTATE", secret: rec.pb, managed: rec.Managed != nil})
			}
			if changed {
				if err := putSecret(tx, ref, rec, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		s.env.Log.Warn("secrets: sweep failed", "err", err)
		return
	}
	for _, e := range events {
		if e.event == "SECRET_DELETE" {
			s.deletePolicy(ctx, e.ref.resource())
		}
		s.notify(ctx, e.secret.GetTopics(), e.event, e.secret, e.version)
		if e.managed {
			if _, err := s.rotateManaged(ctx, e.ref, ""); err != nil {
				s.env.Log.Warn("secrets: managed rotation failed", "secret", e.ref.name(), "err", err)
			}
		}
	}
}

// loop runs the sweep until ctx is done, every interval and on kick.
func (s *Service) loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kickC:
		}
	}
}

// kick asks the loop to sweep now.
func (s *Service) kick() {
	select {
	case s.kickC <- struct{}{}:
	default:
	}
}
