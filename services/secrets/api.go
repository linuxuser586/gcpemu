package secrets

import (
	"context"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// api implements secretmanagerpb.SecretManagerServiceServer for gRPC and,
// through the transcoder, REST.
type api struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer
	s *Service
}

// mutableFields are the Secret fields UpdateSecret may change.
var mutableFields = map[string]bool{
	"labels": true, "annotations": true, "topics": true, "rotation": true,
	"expire_time": true, "ttl": true, "version_aliases": true,
	"version_destroy_ttl": true, "customer_managed_encryption": true,
}

const (
	minRotationLead   = 300 * time.Second
	minRotationPeriod = time.Hour
	maxDuration       = 3153600000 * time.Second // 100 years
	minDestroyTTL     = 24 * time.Hour
	maxDestroyTTL     = 1000 * 24 * time.Hour
	maxAliases        = 50
	maxAnnotationSize = 16 * 1024
)

// CreateSecret creates a secret without versions.
func (a *api) CreateSecret(ctx context.Context, req *secretmanagerpb.CreateSecretRequest) (*secretmanagerpb.Secret, error) {
	s := a.s
	ref, err := s.parseParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.secrets.create", resRoot+ref.parentWith(ref.Project)); err != nil {
		return nil, err
	}
	ref.ID = req.GetSecretId()
	if !secretIDRe.MatchString(ref.ID) {
		return nil, apierr.InvalidArgument("Secret ID %q is invalid: it must be 1-255 characters of letters, numbers, hyphens and underscores.", ref.ID)
	}
	in := req.GetSecret()
	if in == nil {
		in = &secretmanagerpb.Secret{}
	}
	sec := clone(in)
	now := s.env.Clock.Now()
	sec.Name = ref.name()
	sec.CreateTime = timestamppb.New(now)
	sec.Tags = nil
	if err := s.validateSecret(ctx, ref, sec, nil, now); err != nil {
		return nil, err
	}
	if len(sec.GetVersionAliases()) > 0 {
		return nil, apierr.InvalidArgument("Version aliases must refer to existing versions; a new secret has none.")
	}
	rec := &secretRec{pb: sec}
	err = s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsSecrets, ref.key()) {
			return apierr.AlreadyExists("Secret [%s] already exists.", ref.name())
		}
		return putSecret(tx, ref, rec, now)
	})
	if err != nil {
		return nil, err
	}
	out := clone(rec.pb)
	s.notify(ctx, out.GetTopics(), "TOPIC_CONFIGURED", out, nil)
	s.notify(ctx, out.GetTopics(), "SECRET_CREATE", out, nil)
	s.kick()
	return out, nil
}

// validateSecret checks a secret about to be stored and normalizes its
// output-only and input-only fields. old is the stored secret on update.
func (s *Service) validateSecret(ctx context.Context, ref secretRef, sec, old *secretmanagerpb.Secret, now time.Time) error {
	if ref.Location == "" {
		rep := sec.GetReplication()
		switch {
		case rep.GetAutomatic() != nil:
		case rep.GetUserManaged() != nil:
			reps := rep.GetUserManaged().GetReplicas()
			if len(reps) == 0 {
				return apierr.InvalidArgument("A user-managed replication policy needs at least one replica.")
			}
			seen := map[string]bool{}
			for _, r := range reps {
				if !locations.IsRegion(r.GetLocation()) || seen[r.GetLocation()] {
					return apierr.InvalidArgument("Replica location %q is invalid or repeated.", r.GetLocation())
				}
				seen[r.GetLocation()] = true
			}
		default:
			return apierr.InvalidArgument("Secret.replication is required.")
		}
		if sec.GetCustomerManagedEncryption() != nil {
			return apierr.InvalidArgument("Secret.customer_managed_encryption is only valid for regional secrets; set it in the replication policy.")
		}
		if sec.GetSecretType() == secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS {
			return apierr.InvalidArgument("Secrets of type CLOUD_SQL_DB_CREDENTIALS must be regional.")
		}
	} else if sec.GetReplication() != nil {
		return apierr.InvalidArgument("Regional secrets must not set Secret.replication.")
	}
	if old != nil {
		sec.Replication = old.GetReplication()
		sec.SecretType = old.GetSecretType()
	}
	if err := validateLabels(sec.GetLabels()); err != nil {
		return err
	}
	size := 0
	for k, v := range sec.GetAnnotations() {
		size += len(k) + len(v)
	}
	if size > maxAnnotationSize {
		return apierr.InvalidArgument("Secret annotations exceed the 16KiB limit.")
	}
	if err := s.validateTopics(ctx, sec.GetTopics()); err != nil {
		return err
	}
	if err := validateRotation(sec, old, now); err != nil {
		return err
	}
	switch e := sec.GetExpiration().(type) {
	case *secretmanagerpb.Secret_Ttl:
		d := e.Ttl.AsDuration()
		if d <= 0 {
			return apierr.InvalidArgument("Secret.ttl must be positive.")
		}
		sec.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(now.Add(d))}
	case *secretmanagerpb.Secret_ExpireTime:
		if !e.ExpireTime.AsTime().After(now) {
			return apierr.InvalidArgument("Secret.expire_time must be in the future.")
		}
	}
	if d := sec.GetVersionDestroyTtl(); d != nil && (d.AsDuration() < minDestroyTTL || d.AsDuration() > maxDestroyTTL) {
		return apierr.InvalidArgument("Secret.version_destroy_ttl must be between 1 day and 1000 days.")
	}
	if len(sec.GetVersionAliases()) > maxAliases {
		return apierr.InvalidArgument("A secret can have at most %d version aliases.", maxAliases)
	}
	for k := range sec.GetVersionAliases() {
		if !aliasRe.MatchString(k) || k == "latest" || strings.Trim(k, "0123456789") == "" {
			return apierr.InvalidArgument("Version alias %q is invalid.", k)
		}
	}
	sec.Tags = nil
	sec.PolicyMember = nil
	return nil
}

func validateLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return apierr.InvalidArgument("A secret can have at most 64 labels.")
	}
	for k, v := range labels {
		if !labelKeyRe.MatchString(k) || !labelValRe.MatchString(v) {
			return apierr.InvalidArgument("Label %q=%q is invalid.", k, v)
		}
	}
	return nil
}

func (s *Service) validateTopics(ctx context.Context, topics []*secretmanagerpb.Topic) error {
	if len(topics) > 10 {
		return apierr.InvalidArgument("A secret can have at most 10 topics.")
	}
	pub := s.publisher()
	for _, t := range topics {
		parts := strings.Split(t.GetName(), "/")
		if len(parts) != 4 || parts[0] != "projects" || parts[2] != "topics" || parts[1] == "" || parts[3] == "" {
			return apierr.InvalidArgument("Topic name %q is invalid.", t.GetName())
		}
		if pub != nil && !pub.TopicExists(ctx, t.GetName()) {
			return apierr.NotFound("Pub/Sub topic %s not found.", t.GetName())
		}
	}
	return nil
}

func validateRotation(sec, old *secretmanagerpb.Secret, now time.Time) error {
	rot := sec.GetRotation()
	if rot == nil {
		return nil
	}
	if old != nil {
		rot.ManagedRotationStatus = old.GetRotation().GetManagedRotationStatus()
	} else {
		rot.ManagedRotationStatus = nil
	}
	typed := sec.GetSecretType() == secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS
	if (rot.GetNextRotationTime() != nil || rot.GetRotationPeriod() != nil) && len(sec.GetTopics()) == 0 && !typed {
		return apierr.InvalidArgument("A secret with a rotation schedule must have at least one topic.")
	}
	if p := rot.GetRotationPeriod(); p != nil {
		if rot.GetNextRotationTime() == nil {
			return apierr.InvalidArgument("Rotation.next_rotation_time must be set when rotation_period is set.")
		}
		if d := p.AsDuration(); d < minRotationPeriod || d > maxDuration || d%time.Second != 0 {
			return apierr.InvalidArgument("Rotation.rotation_period must be whole seconds between 1 hour and 100 years.")
		}
	}
	if t := rot.GetNextRotationTime(); t != nil {
		unchanged := old != nil && old.GetRotation().GetNextRotationTime().AsTime().Equal(t.AsTime())
		if !unchanged && (t.AsTime().Before(now.Add(minRotationLead)) || t.AsTime().After(now.Add(maxDuration))) {
			return apierr.InvalidArgument("Rotation.next_rotation_time must be at least 5 minutes and at most 100 years in the future.")
		}
	}
	return nil
}

// GetSecret returns a secret's metadata.
func (a *api) GetSecret(ctx context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	ref, err := a.s.parseSecretOnly(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "secretmanager.secrets.get", ref.resource()); err != nil {
		return nil, err
	}
	var out *secretmanagerpb.Secret
	err = a.s.env.Store.View(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err == nil {
			out = rec.pb
		}
		return err
	})
	return out, err
}

// ListSecrets lists the secrets of a project or location.
func (a *api) ListSecrets(ctx context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	s := a.s
	ref, err := s.parseParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.secrets.list", resRoot+ref.parentWith(ref.Project)); err != nil {
		return nil, err
	}
	match, err := parseFilter(req.GetFilter(), secretField, &secretmanagerpb.Secret{})
	if err != nil {
		return nil, err
	}
	var all []*secretmanagerpb.Secret
	err = s.env.Store.View(func(tx store.Tx) error {
		loc := ref.Location
		if loc == "" {
			loc = "global"
		}
		tx.Scan(nsSecrets, ref.Project+"/"+loc+"/", func(k string, _ []byte) bool {
			if rec, err := getSecret(tx, refFromKey(k)); err == nil && match(rec.pb) {
				all = append(all, rec.pb)
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	pg, next, err := page(all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &secretmanagerpb.ListSecretsResponse{Secrets: pg, NextPageToken: next, TotalSize: int32(len(all))}, nil
}

// UpdateSecret patches a secret's mutable fields.
func (a *api) UpdateSecret(ctx context.Context, req *secretmanagerpb.UpdateSecretRequest) (*secretmanagerpb.Secret, error) {
	s := a.s
	in := req.GetSecret()
	if in == nil {
		return nil, apierr.InvalidArgument("Secret is required.")
	}
	ref, err := s.parseSecretOnly(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.secrets.update", ref.resource()); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierr.InvalidArgument("update_mask is required.")
	}
	now := s.env.Clock.Now()
	var out *secretmanagerpb.Secret
	var added []*secretmanagerpb.Topic
	err = s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		if err := checkEtag(in.GetEtag(), rec.pb.GetEtag()); err != nil {
			return err
		}
		old := rec.pb
		sec := clone(old)
		for _, p := range paths {
			p = snakeCase(p)
			field, _, _ := strings.Cut(p, ".")
			if !mutableFields[field] {
				return apierr.InvalidArgument("Invalid update_mask path %q: the field is immutable or does not exist.", p)
			}
			switch field {
			case "labels":
				sec.Labels = in.GetLabels()
			case "annotations":
				sec.Annotations = in.GetAnnotations()
			case "topics":
				sec.Topics = in.GetTopics()
			case "rotation":
				sec.Rotation = in.GetRotation()
			case "expire_time", "ttl":
				sec.Expiration = in.GetExpiration()
			case "version_aliases":
				sec.VersionAliases = in.GetVersionAliases()
			case "version_destroy_ttl":
				sec.VersionDestroyTtl = in.GetVersionDestroyTtl()
			case "customer_managed_encryption":
				if ref.Location == "" {
					return apierr.InvalidArgument("Secret.customer_managed_encryption is only valid for regional secrets.")
				}
				sec.CustomerManagedEncryption = in.GetCustomerManagedEncryption()
			}
		}
		if err := s.validateSecret(ctx, ref, sec, old, now); err != nil {
			return err
		}
		for alias, n := range sec.GetVersionAliases() {
			v, ok := getVersion(tx, ref, n)
			if !ok || v.pb.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
				return apierr.InvalidArgument("Version alias %q refers to version %d, which does not exist or is destroyed.", alias, n)
			}
		}
		was := map[string]bool{}
		for _, t := range old.GetTopics() {
			was[t.GetName()] = true
		}
		for _, t := range sec.GetTopics() {
			if !was[t.GetName()] {
				added = append(added, t)
			}
		}
		rec.pb = sec
		if err := putSecret(tx, ref, rec, now); err != nil {
			return err
		}
		out = clone(sec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.notify(ctx, added, "TOPIC_CONFIGURED", out, nil)
	s.notify(ctx, out.GetTopics(), "SECRET_UPDATE", out, nil)
	s.kick()
	return out, nil
}

// DeleteSecret deletes a secret, its versions and its IAM policy.
func (a *api) DeleteSecret(ctx context.Context, req *secretmanagerpb.DeleteSecretRequest) (*emptypb.Empty, error) {
	s := a.s
	ref, err := s.parseSecretOnly(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "secretmanager.secrets.delete", ref.resource()); err != nil {
		return nil, err
	}
	var gone *secretmanagerpb.Secret
	err = s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		if err := checkEtag(req.GetEtag(), rec.pb.GetEtag()); err != nil {
			return err
		}
		gone = rec.pb
		return deleteSecretData(tx, ref)
	})
	if err != nil {
		return nil, err
	}
	s.deletePolicy(ctx, ref.resource())
	s.notify(ctx, gone.GetTopics(), "SECRET_DELETE", gone, nil)
	return &emptypb.Empty{}, nil
}

// snakeCase converts a camelCase field mask path to snake_case.
func snakeCase(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
