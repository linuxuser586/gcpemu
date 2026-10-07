package gcs

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Buckets (FR-GCS-001).

// mutableBucketFields are the bucket fields patch/update may change.
var mutableBucketFields = map[string]bool{
	"acl": true, "autoclass": true, "billing": true, "cors": true, "defaultEventBasedHold": true,
	"defaultObjectAcl": true, "encryption": true, "iamConfiguration": true, "ipFilter": true,
	"labels": true, "lifecycle": true, "logging": true, "retentionPolicy": true, "rpo": true,
	"softDeletePolicy": true, "storageClass": true, "versioning": true, "website": true,
}

const defaultSoftDeleteSeconds = 7 * 24 * 3600

func (s *Service) now() time.Time { return s.env.Clock.Now().UTC() }

// loadBucket reads a bucket record.
func loadBucket(tx store.Tx, name string) (*bucketRec, error) {
	var rec bucketRec
	if err := store.GetJSON(tx, nsBuckets, name, &rec); err != nil {
		if err == store.ErrNotFound {
			return nil, errBucketNotFound()
		}
		return nil, err
	}
	return &rec, nil
}

func (s *Service) getBucketRec(name string) (*bucketRec, error) {
	var rec *bucketRec
	err := s.env.Store.View(func(tx store.Tx) error {
		var err error
		rec, err = loadBucket(tx, name)
		return err
	})
	return rec, err
}

func (s *Service) insertBucket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	proj := q.Get("project")
	if proj == "" {
		apierr.Write(w, errRequired("project"))
		return
	}
	var b storage.Bucket
	if err := decodeJSON(r, &b); err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.createBucket(r.Context(), proj, &b, q.Get("enableObjectRetention") == "true")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderBucket(rec, baseURL(r)))
}

// createBucket validates and stores a new bucket.
func (s *Service) createBucket(ctx context.Context, proj string, b *storage.Bucket, objectRetention bool) (*bucketRec, error) {
	if err := s.env.EnsureProject(proj); err != nil {
		return nil, err
	}
	if err := s.check(ctx, "storage.buckets.create", projectResource(proj)); err != nil {
		return nil, err
	}
	if b.Name == "" {
		return nil, apierr.InvalidArgument("Required").WithLegacy("required")
	}
	if !validBucketName(b.Name) {
		return nil, errInvalid("Invalid bucket name: '%s'", b.Name)
	}
	now := s.now()
	b.Location = strings.ToUpper(b.Location)
	if b.Location == "" {
		b.Location = "US"
	}
	b.LocationType = locationType(b.Location, b.CustomPlacementConfig != nil && len(b.CustomPlacementConfig.DataLocations) > 0)
	if b.LocationType == "" {
		return nil, errInvalid("The specified location constraint is not valid.")
	}
	b.StorageClass = strings.ToUpper(b.StorageClass)
	if b.StorageClass == "" {
		b.StorageClass = "STANDARD"
	}
	if !validStorageClass(b.StorageClass) {
		return nil, errInvalid("The specified storage class is not valid.")
	}
	if err := validateLifecycle(b.Lifecycle); err != nil {
		return nil, err
	}
	b.Kind = "storage#bucket"
	b.Id = b.Name
	b.ProjectNumber = uint64(project.Number(proj))
	b.TimeCreated, b.Updated = ts(now), ts(now)
	b.Metageneration = 1
	b.Generation = now.UnixMicro()
	b.Acl, b.DefaultObjectAcl, b.Owner = nil, nil, nil
	b.SelfLink, b.Etag = "", ""
	if b.Rpo == "" && b.LocationType != "region" {
		b.Rpo = "DEFAULT"
	}
	if b.SoftDeletePolicy == nil {
		b.SoftDeletePolicy = &storage.BucketSoftDeletePolicy{RetentionDurationSeconds: defaultSoftDeleteSeconds}
	}
	b.SoftDeletePolicy.EffectiveTime = ts(now)
	if b.RetentionPolicy != nil {
		b.RetentionPolicy.EffectiveTime = ts(now)
		b.RetentionPolicy.IsLocked = false
	}
	if objectRetention {
		b.ObjectRetention = &storage.BucketObjectRetention{Mode: "Enabled"}
	} else {
		b.ObjectRetention = nil
	}
	normalizeIAMConfig(b, nil, now)

	rec := &bucketRec{Bucket: b, Project: proj}
	err := s.env.Store.Update(func(tx store.Tx) error {
		var existing bucketRec
		if store.GetJSON(tx, nsBuckets, b.Name, &existing) == nil {
			if existing.Project == proj {
				return apierr.AlreadyExists("Your previous request to create the named bucket succeeded and you already own it.")
			}
			return apierr.AlreadyExists("The requested bucket name is not available. The bucket namespace is shared by all users of the system. Please select a different name and try again.")
		}
		return store.PutJSON(tx, nsBuckets, b.Name, rec)
	})
	if err != nil {
		return nil, err
	}
	if svc, ok := s.env.Lookup("iam"); ok {
		if rp, ok := svc.(emu.IAMResourceParents); ok {
			if err := rp.SetResourceParent(ctx, bucketResource(b.Name), "projects/"+proj); err != nil {
				s.log.Warn("set bucket IAM parent failed", "bucket", b.Name, "err", err)
			}
		}
	}
	return rec, nil
}

// normalizeIAMConfig keeps uniformBucketLevelAccess and the legacy
// bucketPolicyOnly alias in sync and sets lockedTime when UBLA is enabled.
func normalizeIAMConfig(b *storage.Bucket, prev *storage.Bucket, now time.Time) {
	ic := b.IamConfiguration
	if ic == nil {
		ic = &storage.BucketIamConfiguration{}
		b.IamConfiguration = ic
	}
	enabled := false
	switch {
	case ic.UniformBucketLevelAccess != nil:
		enabled = ic.UniformBucketLevelAccess.Enabled
	case ic.BucketPolicyOnly != nil:
		enabled = ic.BucketPolicyOnly.Enabled
	}
	wasEnabled := false
	locked := ""
	if prev != nil && prev.IamConfiguration != nil && prev.IamConfiguration.UniformBucketLevelAccess != nil {
		wasEnabled = prev.IamConfiguration.UniformBucketLevelAccess.Enabled
		locked = prev.IamConfiguration.UniformBucketLevelAccess.LockedTime
	}
	if enabled && (!wasEnabled || locked == "") {
		locked = ts(now.Add(90 * 24 * time.Hour))
	}
	if !enabled {
		locked = ""
	}
	ic.UniformBucketLevelAccess = &storage.BucketIamConfigurationUniformBucketLevelAccess{Enabled: enabled, LockedTime: locked}
	ic.BucketPolicyOnly = &storage.BucketIamConfigurationBucketPolicyOnly{Enabled: enabled, LockedTime: locked}
	if ic.PublicAccessPrevention == "" || ic.PublicAccessPrevention == "unspecified" {
		ic.PublicAccessPrevention = "inherited"
	}
}

func (s *Service) getBucket(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.check(r.Context(), "storage.buckets.get", bucketResource(name)); err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(r.URL.Query(), "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.getBucketRec(name)
	if err == nil {
		err = c.checkBucket(rec.Bucket)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderBucket(rec, baseURL(r)))
}

func (s *Service) listBuckets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	proj := q.Get("project")
	if proj == "" {
		apierr.Write(w, errRequired("project"))
		return
	}
	if err := s.env.EnsureProject(proj); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "storage.buckets.list", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	max := pageSize(q.Get("maxResults"), 1000)
	marker, _ := decodeToken(q.Get("pageToken"))
	prefix := q.Get("prefix")
	out := &storage.Buckets{Kind: "storage#buckets"}
	base := baseURL(r)
	err := s.env.Store.View(func(tx store.Tx) error {
		var ierr error
		tx.Scan(nsBuckets, prefix, func(key string, val []byte) bool {
			if marker != "" && key <= marker {
				return true
			}
			var rec bucketRec
			if ierr = jsonUnmarshal(val, &rec); ierr != nil {
				return false
			}
			if rec.Project != proj && strconv.FormatUint(rec.Bucket.ProjectNumber, 10) != proj {
				return true
			}
			if len(out.Items) == max {
				out.NextPageToken = encodeToken(out.Items[len(out.Items)-1].Name)
				return false
			}
			out.Items = append(out.Items, renderBucket(&rec, base))
			return true
		})
		return ierr
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) patchBucket(w http.ResponseWriter, r *http.Request, name string, replace bool) {
	if err := s.check(r.Context(), "storage.buckets.update", bucketResource(name)); err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(r.URL.Query(), "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	body, _, err := readJSONBody(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var out *bucketRec
	err = s.env.Store.Update(func(tx store.Tx) error {
		rec, err := loadBucket(tx, name)
		if err != nil {
			return err
		}
		if err := c.checkBucket(rec.Bucket); err != nil {
			return err
		}
		prev := rec.Bucket
		var nb storage.Bucket
		if err := applyPatch(prev, body, mutableBucketFields, replace, &nb); err != nil {
			return err
		}
		if err := s.validateBucketUpdate(prev, &nb); err != nil {
			return err
		}
		now := s.now()
		nb.Metageneration = prev.Metageneration + 1
		nb.Updated = ts(now)
		nb.Acl, nb.DefaultObjectAcl = nil, nil
		normalizeIAMConfig(&nb, prev, now)
		if nb.RetentionPolicy != nil {
			if prev.RetentionPolicy == nil || prev.RetentionPolicy.RetentionPeriod != nb.RetentionPolicy.RetentionPeriod {
				nb.RetentionPolicy.EffectiveTime = ts(now)
			}
			if prev.RetentionPolicy != nil {
				nb.RetentionPolicy.IsLocked = prev.RetentionPolicy.IsLocked
			} else {
				nb.RetentionPolicy.IsLocked = false
			}
		}
		if nb.SoftDeletePolicy == nil {
			nb.SoftDeletePolicy = &storage.BucketSoftDeletePolicy{RetentionDurationSeconds: defaultSoftDeleteSeconds}
		}
		if prev.SoftDeletePolicy == nil || prev.SoftDeletePolicy.RetentionDurationSeconds != nb.SoftDeletePolicy.RetentionDurationSeconds {
			nb.SoftDeletePolicy.EffectiveTime = ts(now)
		}
		nb.StorageClass = strings.ToUpper(nb.StorageClass)
		if nb.StorageClass == "" {
			nb.StorageClass = prev.StorageClass
		}
		rec.Bucket = &nb
		out = rec
		return store.PutJSON(tx, nsBuckets, name, rec)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderBucket(out, baseURL(r)))
}

// validateBucketUpdate enforces update rules: locked retention policies
// cannot be removed or shortened, storage class and lifecycle must be valid.
func (s *Service) validateBucketUpdate(prev, nb *storage.Bucket) error {
	if p := prev.RetentionPolicy; p != nil && p.IsLocked {
		if nb.RetentionPolicy == nil || nb.RetentionPolicy.RetentionPeriod < p.RetentionPeriod {
			return errForbidden("Cannot reduce retention duration or remove a locked Retention Policy for bucket '%s'.", prev.Name)
		}
	}
	if nb.StorageClass != "" && !validStorageClass(strings.ToUpper(nb.StorageClass)) {
		return errInvalid("The specified storage class is not valid.")
	}
	return validateLifecycle(nb.Lifecycle)
}

func (s *Service) deleteBucket(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.check(r.Context(), "storage.buckets.delete", bucketResource(name)); err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(r.URL.Query(), "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.removeBucket(r.Context(), name, c); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeBucket deletes an empty bucket with its notification configs,
// pending uploads and IAM policy (FR-CORE-026).
func (s *Service) removeBucket(ctx context.Context, name string, c conds) error {
	var uploadIDs []string
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, err := loadBucket(tx, name)
		if err != nil {
			return err
		}
		if err := c.checkBucket(rec.Bucket); err != nil {
			return err
		}
		if store.HasPrefix(tx, nsObjects, objPrefix(name, "")) || store.HasPrefix(tx, nsVersions, objPrefix(name, "")) {
			return errConflict("The bucket you tried to delete is not empty.")
		}
		var keys []string
		tx.Scan(nsNotifs, objPrefix(name, ""), func(k string, _ []byte) bool { keys = append(keys, k); return true })
		for _, k := range keys {
			if err := tx.Delete(nsNotifs, k); err != nil {
				return err
			}
		}
		tx.Scan(nsUploads, "", func(k string, v []byte) bool {
			var u uploadRec
			if jsonUnmarshal(v, &u) == nil && u.Bucket == name {
				uploadIDs = append(uploadIDs, k)
			}
			return true
		})
		for _, id := range uploadIDs {
			if err := tx.Delete(nsUploads, id); err != nil {
				return err
			}
		}
		_ = tx.Delete(nsIAM, name)
		return tx.Delete(nsBuckets, name)
	})
	if err != nil {
		return err
	}
	for _, id := range uploadIDs {
		s.dropSession(id)
	}
	if ps := s.policyStore(); ps != nil {
		_ = ps.DeletePolicy(ctx, bucketResource(name))
	}
	return nil
}

func (s *Service) lockRetentionPolicy(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.check(r.Context(), "storage.buckets.update", bucketResource(name)); err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(r.URL.Query(), "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if c.MetagenMatch == nil {
		apierr.Write(w, errRequired("ifMetagenerationMatch"))
		return
	}
	var out *bucketRec
	err = s.env.Store.Update(func(tx store.Tx) error {
		rec, err := loadBucket(tx, name)
		if err != nil {
			return err
		}
		if err := c.checkBucket(rec.Bucket); err != nil {
			return err
		}
		if rec.Bucket.RetentionPolicy == nil {
			return errInvalid("Bucket '%s' does not have a retention policy to lock.", name)
		}
		rec.Bucket.RetentionPolicy.IsLocked = true
		rec.Bucket.Metageneration++
		rec.Bucket.Updated = ts(s.now())
		out = rec
		return store.PutJSON(tx, nsBuckets, name, rec)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderBucket(out, baseURL(r)))
}

// getServiceAccount returns the project's GCS service agent.
func (s *Service) getServiceAccount(w http.ResponseWriter, r *http.Request, proj string) {
	if err := s.env.EnsureProject(proj); err != nil {
		apierr.Write(w, err)
		return
	}
	num := proj
	if !isDigits(proj) {
		num = project.NumberString(proj)
	}
	writeJSON(w, http.StatusOK, &storage.ServiceAccount{
		Kind:         "storage#serviceAccount",
		EmailAddress: "service-" + num + "@gs-project-accounts.iam.gserviceaccount.com",
	})
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// pageSize parses a maxResults/pageSize value, capping it at def.
func pageSize(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > def {
		return def
	}
	return n
}

func encodeToken(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decodeToken(t string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(t)
	return string(b), err
}
