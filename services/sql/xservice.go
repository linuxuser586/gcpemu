package sql

import (
	"context"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

var _ emu.SQLUsers = (*Service)(nil)

// InstanceRegion implements emu.SQLUsers.
func (s *Service) InstanceRegion(project, instance string) (string, error) {
	var region string
	err := s.env.Store.View(func(tx store.Tx) error {
		rec, ok := getInstance(tx, project, instance)
		if !ok {
			return apierr.NotFound("Cloud SQL instance %s does not exist in project %s.", instance, project)
		}
		region = rec.Instance.Region
		return nil
	})
	return region, err
}

// SetUserPassword implements emu.SQLUsers: it sets a built-in user's
// password as users.update would, without an Operation.
func (s *Service) SetUserPassword(ctx context.Context, project, instance, user, password string) error {
	var rec *instanceRecord
	err := s.env.Store.View(func(tx store.Tx) error {
		var ok bool
		if rec, ok = getInstance(tx, project, instance); !ok {
			return apierr.NotFound("Cloud SQL instance %s does not exist in project %s.", instance, project)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := requireRunnable(rec); err != nil {
		return err
	}
	ur, ok := s.findUser(project, instance, user)
	if !ok {
		return errUserNotFound(user)
	}
	if ur.User.Type != "" && ur.User.Type != "BUILT_IN" {
		return apierr.FailedPrecondition("User %s on Cloud SQL instance %s is not a built-in user.", user, instance)
	}
	return s.saveUser(ctx, project, instance, ur, password)
}
