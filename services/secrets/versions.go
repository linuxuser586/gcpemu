package secrets

import (
	"context"
	"hash/crc32"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const maxPayload = 64 * 1024

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// AddSecretVersion adds an ENABLED version holding the payload.
func (a *api) AddSecretVersion(ctx context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	s := a.s
	ref, err := s.parseSecretOnly(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.versions.add", ref.resource()); err != nil {
		return nil, err
	}
	data := req.GetPayload().GetData()
	if len(data) > maxPayload {
		return nil, apierr.InvalidArgument("Secret payload exceeds the 64KiB limit.")
	}
	crc := int64(crc32.Checksum(data, castagnoli))
	if c := req.GetPayload().DataCrc32C; c != nil && *c != crc {
		return nil, apierr.InvalidArgument("Checksum mismatch: data_crc32c does not match the payload.").WithReason(errDom, "DATA_CORRUPTION")
	}
	var typed bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		typed = err == nil && rec.pb.GetSecretType() == secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS
		return nil
	})
	if typed {
		return nil, apierr.FailedPrecondition("Versions of a CLOUD_SQL_DB_CREDENTIALS secret are added by managed rotation only.")
	}
	sec, v, err := s.addVersion(ref, data, req.GetPayload().DataCrc32C != nil)
	if err != nil {
		return nil, err
	}
	s.notify(ctx, sec.GetTopics(), "SECRET_VERSION_ADD", sec, v)
	return v, nil
}

// addVersion stores a new ENABLED version and returns the secret and the
// version.
func (s *Service) addVersion(ref secretRef, data []byte, clientCRC bool) (*secretmanagerpb.Secret, *secretmanagerpb.SecretVersion, error) {
	now := s.env.Clock.Now()
	var sec *secretmanagerpb.Secret
	var out *secretmanagerpb.SecretVersion
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		rec.Last++
		v := &secretmanagerpb.SecretVersion{
			Name:                           ref.versionName(rec.Last),
			CreateTime:                     timestamppb.New(now),
			State:                          secretmanagerpb.SecretVersion_ENABLED,
			ReplicationStatus:              replicationStatus(rec.pb),
			ClientSpecifiedPayloadChecksum: clientCRC,
		}
		if c := rec.pb.GetCustomerManagedEncryption(); c != nil {
			v.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: c.GetKmsKeyName() + "/cryptoKeyVersions/1"}
		}
		vr := &versionRec{pb: v, Payload: append([]byte(nil), data...), CRC: int64(crc32.Checksum(data, castagnoli))}
		if err := putVersion(tx, ref, rec.Last, vr, now); err != nil {
			return err
		}
		if err := putSecret(tx, ref, rec, now); err != nil {
			return err
		}
		sec, out = rec.pb, vr.pb
		return nil
	})
	return sec, out, err
}

// replicationStatus mirrors the secret's replication policy; CMEK is
// Recorded.
func replicationStatus(sec *secretmanagerpb.Secret) *secretmanagerpb.ReplicationStatus {
	rep := sec.GetReplication()
	switch {
	case rep.GetAutomatic() != nil:
		st := &secretmanagerpb.ReplicationStatus_AutomaticStatus{}
		if c := rep.GetAutomatic().GetCustomerManagedEncryption(); c != nil {
			st.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: c.GetKmsKeyName() + "/cryptoKeyVersions/1"}
		}
		return &secretmanagerpb.ReplicationStatus{ReplicationStatus: &secretmanagerpb.ReplicationStatus_Automatic{Automatic: st}}
	case rep.GetUserManaged() != nil:
		st := &secretmanagerpb.ReplicationStatus_UserManagedStatus{}
		for _, r := range rep.GetUserManaged().GetReplicas() {
			rs := &secretmanagerpb.ReplicationStatus_UserManagedStatus_ReplicaStatus{Location: r.GetLocation()}
			if c := r.GetCustomerManagedEncryption(); c != nil {
				rs.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: c.GetKmsKeyName() + "/cryptoKeyVersions/1"}
			}
			st.Replicas = append(st.Replicas, rs)
		}
		return &secretmanagerpb.ReplicationStatus{ReplicationStatus: &secretmanagerpb.ReplicationStatus_UserManaged{UserManaged: st}}
	}
	return nil
}

// lookupVersion resolves a version name inside tx.
func (s *Service) lookupVersion(tx store.Tx, ref secretRef, id, name string) (*secretRec, *versionRec, int64, error) {
	rec, err := getSecret(tx, ref)
	if err != nil {
		return nil, nil, 0, err
	}
	n, ok := resolveVersion(tx, ref, rec, id)
	if !ok {
		return nil, nil, 0, versionNotFound(name)
	}
	v, ok := getVersion(tx, ref, n)
	if !ok {
		return nil, nil, 0, versionNotFound(name)
	}
	return rec, v, n, nil
}

// GetSecretVersion returns a version's metadata.
func (a *api) GetSecretVersion(ctx context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	s := a.s
	ref, id, err := s.parseVersion(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.versions.get", ref.versionResource(id)); err != nil {
		return nil, err
	}
	var out *secretmanagerpb.SecretVersion
	err = s.env.Store.View(func(tx store.Tx) error {
		_, v, _, err := s.lookupVersion(tx, ref, id, req.GetName())
		if err == nil {
			out = v.pb
		}
		return err
	})
	return out, err
}

// ListSecretVersions lists a secret's versions, newest first.
func (a *api) ListSecretVersions(ctx context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	s := a.s
	ref, err := s.parseSecretOnly(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.versions.list", ref.resource()); err != nil {
		return nil, err
	}
	match, err := parseFilter(req.GetFilter(), versionField, &secretmanagerpb.SecretVersion{})
	if err != nil {
		return nil, err
	}
	var all []*secretmanagerpb.SecretVersion
	err = s.env.Store.View(func(tx store.Tx) error {
		if _, err := getSecret(tx, ref); err != nil {
			return err
		}
		for _, v := range listVersions(tx, ref) {
			if match(v.pb) {
				all = append(all, v.pb)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pg, next, err := page(all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &secretmanagerpb.ListSecretVersionsResponse{Versions: pg, NextPageToken: next, TotalSize: int32(len(all))}, nil
}

// AccessSecretVersion returns an ENABLED version's payload.
func (a *api) AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	s := a.s
	ref, id, err := s.parseVersion(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.versions.access", ref.versionResource(id)); err != nil {
		return nil, err
	}
	var out *secretmanagerpb.AccessSecretVersionResponse
	err = s.env.Store.View(func(tx store.Tx) error {
		_, v, _, err := s.lookupVersion(tx, ref, id, req.GetName())
		if err != nil {
			return err
		}
		if st := v.pb.GetState(); st != secretmanagerpb.SecretVersion_ENABLED {
			return apierr.FailedPrecondition("Secret Version [%s] is in %s state.", v.pb.GetName(), st)
		}
		crc := v.CRC
		out = &secretmanagerpb.AccessSecretVersionResponse{
			Name:    v.pb.GetName(),
			Payload: &secretmanagerpb.SecretPayload{Data: v.Payload, DataCrc32C: &crc},
		}
		return nil
	})
	return out, err
}

// EnableSecretVersion enables a DISABLED version, cancelling a scheduled
// destruction.
func (a *api) EnableSecretVersion(ctx context.Context, req *secretmanagerpb.EnableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return a.s.changeVersion(ctx, req.GetName(), req.GetEtag(), "secretmanager.versions.enable", "SECRET_VERSION_ENABLE",
		func(sec *secretmanagerpb.Secret, v *versionRec, now time.Time) error {
			if v.pb.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
				return apierr.FailedPrecondition("Secret Version [%s] is destroyed and cannot be enabled.", v.pb.GetName())
			}
			v.pb.State = secretmanagerpb.SecretVersion_ENABLED
			v.pb.ScheduledDestroyTime = nil
			return nil
		})
}

// DisableSecretVersion disables a version.
func (a *api) DisableSecretVersion(ctx context.Context, req *secretmanagerpb.DisableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return a.s.changeVersion(ctx, req.GetName(), req.GetEtag(), "secretmanager.versions.disable", "SECRET_VERSION_DISABLE",
		func(sec *secretmanagerpb.Secret, v *versionRec, now time.Time) error {
			if v.pb.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
				return apierr.FailedPrecondition("Secret Version [%s] is destroyed and cannot be disabled.", v.pb.GetName())
			}
			v.pb.State = secretmanagerpb.SecretVersion_DISABLED
			return nil
		})
}

// DestroySecretVersion destroys a version's payload, or with the secret's
// version_destroy_ttl disables it and schedules the destruction.
func (a *api) DestroySecretVersion(ctx context.Context, req *secretmanagerpb.DestroySecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return a.s.changeVersion(ctx, req.GetName(), req.GetEtag(), "secretmanager.versions.destroy", "",
		func(sec *secretmanagerpb.Secret, v *versionRec, now time.Time) error {
			if v.pb.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
				return apierr.FailedPrecondition("Secret Version [%s] is already destroyed.", v.pb.GetName())
			}
			if ttl := sec.GetVersionDestroyTtl(); ttl != nil {
				v.pb.State = secretmanagerpb.SecretVersion_DISABLED
				v.pb.ScheduledDestroyTime = timestamppb.New(now.Add(ttl.AsDuration()))
				return nil
			}
			destroy(v, now)
			return nil
		})
}

// destroy wipes a version's payload.
func destroy(v *versionRec, now time.Time) {
	v.pb.State = secretmanagerpb.SecretVersion_DESTROYED
	v.pb.DestroyTime = timestamppb.New(now)
	v.pb.ScheduledDestroyTime = nil
	v.Payload, v.CRC = nil, 0
}

// changeVersion applies a state change to a version and notifies the
// secret's topics with event (or the destroy event that applies).
func (s *Service) changeVersion(ctx context.Context, name, reqEtag, perm, event string, f func(*secretmanagerpb.Secret, *versionRec, time.Time) error) (*secretmanagerpb.SecretVersion, error) {
	ref, id, err := s.parseVersion(name)
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, perm, ref.versionResource(id)); err != nil {
		return nil, err
	}
	now := s.env.Clock.Now()
	var sec *secretmanagerpb.Secret
	var out *secretmanagerpb.SecretVersion
	err = s.env.Store.Update(func(tx store.Tx) error {
		rec, v, n, err := s.lookupVersion(tx, ref, id, name)
		if err != nil {
			return err
		}
		if err := checkEtag(reqEtag, v.pb.GetEtag()); err != nil {
			return err
		}
		if err := f(rec.pb, v, now); err != nil {
			return err
		}
		if err := putVersion(tx, ref, n, v, now); err != nil {
			return err
		}
		if v.pb.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
			for alias, an := range rec.pb.GetVersionAliases() {
				if an == n {
					delete(rec.pb.VersionAliases, alias)
				}
			}
			if err := putSecret(tx, ref, rec, now); err != nil {
				return err
			}
		}
		sec, out = rec.pb, v.pb
		return nil
	})
	if err != nil {
		return nil, err
	}
	if event == "" {
		event = "SECRET_VERSION_DESTROY"
		if out.GetScheduledDestroyTime() != nil {
			event = "SECRET_VERSION_DESTROY_SCHEDULED"
		}
	}
	s.notify(ctx, sec.GetTopics(), event, sec, out)
	s.kick()
	return out, nil
}
