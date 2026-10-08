package secrets

import (
	"context"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Managed rotation of Cloud SQL single-user credentials. A regional secret
// of type CLOUD_SQL_DB_CREDENTIALS in the region of a Cloud SQL instance
// holds the password of one built-in user. Each rotation generates a
// password (or uses the one given when it is enabled), sets it on the
// Cloud SQL instance through the sql Service and adds it as the secret's
// newest version, whose payload is the password alone. A failed rotation
// leaves managed_rotation_status INACTIVE with the error; RotateSecret
// retries it. The secret's own identity is not checked for Cloud SQL
// permissions.

func (s *Service) sqlUsers() emu.SQLUsers {
	if svc, ok := s.env.Lookup("sql"); ok {
		if u, ok := svc.(emu.SQLUsers); ok {
			return u
		}
	}
	return nil
}

// EnableManagedRotation starts managed rotation and performs the first
// rotation.
func (a *api) EnableManagedRotation(ctx context.Context, req *secretmanagerpb.EnableManagedRotationRequest) (*secretmanagerpb.SecretVersion, error) {
	s := a.s
	ref, err := s.parseSecretOnly(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.versions.add", ref.resource()); err != nil {
		return nil, err
	}
	creds := req.GetCloudSqlSingleUserCredentials()
	if creds.GetInstanceId() == "" || creds.GetUsername() == "" {
		return nil, apierr.InvalidArgument("cloud_sql_single_user_credentials.instance_id and username are required.")
	}
	users := s.sqlUsers()
	if users == nil {
		return nil, apierr.FailedPrecondition("Managed rotation needs the Cloud SQL Service, which is not running.")
	}
	err = s.env.Store.View(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		if ref.Location == "" || rec.pb.GetSecretType() != secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS {
			return apierr.FailedPrecondition("Managed rotation needs a regional secret of type CLOUD_SQL_DB_CREDENTIALS.")
		}
		if rec.Managed != nil {
			return apierr.FailedPrecondition("Managed rotation is already enabled for secret [%s]; use RotateSecret.", ref.name())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	region, err := users.InstanceRegion(ref.Project, creds.GetInstanceId())
	if err != nil {
		return nil, apierr.InvalidArgument("Cloud SQL instance %s: %v", creds.GetInstanceId(), err)
	}
	if region != ref.Location {
		return nil, apierr.InvalidArgument("Cloud SQL instance %s is in %s; the secret must be in the same region, not %s.", creds.GetInstanceId(), region, ref.Location)
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		rec.Managed = &managedCreds{Instance: creds.GetInstanceId(), Username: creds.GetUsername()}
		return putSecret(tx, ref, rec, s.env.Clock.Now())
	})
	if err != nil {
		return nil, err
	}
	return s.rotateManaged(ctx, ref, creds.GetPassword())
}

// RotateSecret performs a managed rotation now.
func (a *api) RotateSecret(ctx context.Context, req *secretmanagerpb.RotateSecretRequest) (*secretmanagerpb.SecretVersion, error) {
	s := a.s
	ref, err := s.parseSecretOnly(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.versions.add", ref.resource()); err != nil {
		return nil, err
	}
	err = s.env.Store.View(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err == nil && rec.Managed == nil {
			err = apierr.FailedPrecondition("Managed rotation is not enabled for secret [%s].", ref.name())
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.rotateManaged(ctx, ref, "")
}

// rotateManaged sets a new password on the Cloud SQL user, adds it as a
// version and records the outcome in managed_rotation_status.
func (s *Service) rotateManaged(ctx context.Context, ref secretRef, password string) (*secretmanagerpb.SecretVersion, error) {
	var creds managedCreds
	err := s.env.Store.View(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err == nil && rec.Managed != nil {
			creds = *rec.Managed
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if password == "" {
		password = s.env.IDs.Hex(16)
	}
	users := s.sqlUsers()
	if users == nil {
		err = apierr.FailedPrecondition("the Cloud SQL Service is not running")
	} else {
		err = users.SetUserPassword(context.WithoutCancel(ctx), ref.Project, creds.Instance, creds.Username, password)
	}
	var v *secretmanagerpb.SecretVersion
	var sec *secretmanagerpb.Secret
	if err == nil {
		sec, v, err = s.addVersion(ref, []byte(password), false)
	}
	st := &secretmanagerpb.Rotation_ManagedRotationStatus{State: secretmanagerpb.Rotation_ManagedRotationStatus_ACTIVE}
	if err != nil {
		ae := apierr.From(err)
		st = &secretmanagerpb.Rotation_ManagedRotationStatus{
			State: secretmanagerpb.Rotation_ManagedRotationStatus_INACTIVE,
			Error: &status.Status{Code: int32(ae.Code), Message: "Rotation failed: " + ae.Message},
		}
	}
	_ = s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		if rec.pb.Rotation == nil {
			rec.pb.Rotation = &secretmanagerpb.Rotation{}
		}
		rec.pb.Rotation.ManagedRotationStatus = st
		return putSecret(tx, ref, rec, s.env.Clock.Now())
	})
	if err != nil {
		if ae := apierr.From(err); ae.Code == codes.NotFound {
			return nil, apierr.FailedPrecondition("Rotation failed: %s", ae.Message)
		}
		return nil, err
	}
	s.notify(ctx, sec.GetTopics(), "SECRET_VERSION_ADD", sec, v)
	return v, nil
}
