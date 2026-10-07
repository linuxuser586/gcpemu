package gcs

import (
	"context"
	"strings"
	"time"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Lifecycle rules (FR-GCS-008), evaluated against the emulator clock so
// `gcpemu time advance` makes objects age.

const day = 24 * time.Hour

func validateLifecycle(l *storage.BucketLifecycle) error {
	if l == nil {
		return nil
	}
	for _, r := range l.Rule {
		if r == nil || r.Action == nil {
			return errInvalid("A lifecycle rule must specify an action.")
		}
		switch r.Action.Type {
		case "Delete", "AbortIncompleteMultipartUpload":
		case "SetStorageClass":
			if !validStorageClass(strings.ToUpper(r.Action.StorageClass)) {
				return errInvalid("The specified storage class is not valid.")
			}
		default:
			return errInvalid("Invalid lifecycle action type: %s", r.Action.Type)
		}
		if r.Condition == nil {
			return errInvalid("A lifecycle rule must specify at least one condition.")
		}
		for _, d := range []string{r.Condition.CreatedBefore, r.Condition.CustomTimeBefore, r.Condition.NoncurrentTimeBefore} {
			if d != "" {
				if _, err := time.Parse("2006-01-02", d); err != nil {
					return errInvalid("Invalid date in lifecycle condition: %s", d)
				}
			}
		}
	}
	return nil
}

// lcObject is an object generation under lifecycle evaluation.
type lcObject struct {
	rec    *objectRec
	live   bool
	newer  int64 // number of newer generations of the same name
	noncur time.Time
}

func date(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

// matches reports whether all of the rule's conditions hold for o.
func matches(c *storage.BucketLifecycleRuleCondition, o *lcObject, now time.Time) bool {
	obj := o.rec.Object
	created := parseTS(obj.TimeCreated)
	if c.Age != nil && now.Sub(created) < time.Duration(*c.Age)*day {
		return false
	}
	if c.CreatedBefore != "" && !created.Before(date(c.CreatedBefore)) {
		return false
	}
	if c.IsLive != nil && *c.IsLive != o.live {
		return false
	}
	if c.CustomTimeBefore != "" && (obj.CustomTime == "" || !parseTS(obj.CustomTime).Before(date(c.CustomTimeBefore))) {
		return false
	}
	if c.DaysSinceCustomTime > 0 && (obj.CustomTime == "" || now.Sub(parseTS(obj.CustomTime)) < time.Duration(c.DaysSinceCustomTime)*day) {
		return false
	}
	if c.DaysSinceNoncurrentTime > 0 && (o.live || now.Sub(o.noncur) < time.Duration(c.DaysSinceNoncurrentTime)*day) {
		return false
	}
	if c.NoncurrentTimeBefore != "" && (o.live || !o.noncur.Before(date(c.NoncurrentTimeBefore))) {
		return false
	}
	if c.NumNewerVersions > 0 && (o.live || o.newer < c.NumNewerVersions) {
		return false
	}
	if len(c.MatchesStorageClass) > 0 && !contains(c.MatchesStorageClass, obj.StorageClass) {
		return false
	}
	if len(c.MatchesPrefix) > 0 && !anyAffix(c.MatchesPrefix, obj.Name, strings.HasPrefix) {
		return false
	}
	if len(c.MatchesSuffix) > 0 && !anyAffix(c.MatchesSuffix, obj.Name, strings.HasSuffix) {
		return false
	}
	if c.SizeAboveBytes > 0 && int64(obj.Size) <= c.SizeAboveBytes {
		return false
	}
	if c.SizeBelowBytes > 0 && int64(obj.Size) >= c.SizeBelowBytes {
		return false
	}
	return true
}

func anyAffix(list []string, name string, f func(string, string) bool) bool {
	for _, p := range list {
		if f(name, p) {
			return true
		}
	}
	return false
}

// EvaluateLifecycle applies every bucket's lifecycle rules once, using the
// emulator clock. The core can call it after `gcpemu time advance`; it also
// runs periodically in the background.
func (s *Service) EvaluateLifecycle(ctx context.Context) error {
	if s.blobs == nil {
		return nil
	}
	var buckets []*bucketRec
	err := s.env.Store.View(func(tx store.Tx) error {
		all, err := store.ListJSON[*bucketRec](tx, nsBuckets, "")
		for _, b := range all {
			if b.Bucket.Lifecycle != nil && len(b.Bucket.Lifecycle.Rule) > 0 {
				buckets = append(buckets, b)
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, b := range buckets {
		if err := s.evaluateBucket(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) evaluateBucket(ctx context.Context, b *bucketRec) error {
	name := b.Bucket.Name
	var objs []*lcObject
	err := s.env.Store.View(func(tx store.Tx) error {
		live, err := store.ListJSON[*objectRec](tx, nsObjects, objPrefix(name, ""))
		if err != nil {
			return err
		}
		vers, err := store.ListJSON[*objectRec](tx, nsVersions, objPrefix(name, ""))
		if err != nil {
			return err
		}
		liveNames := map[string]bool{}
		for _, r := range live {
			liveNames[r.Object.Name] = true
			objs = append(objs, &lcObject{rec: r, live: true})
		}
		// Versions are keyed by name then ascending generation, so the
		// number of newer generations is the count after this one.
		for i, r := range vers {
			n := int64(0)
			for j := i + 1; j < len(vers) && vers[j].Object.Name == r.Object.Name; j++ {
				n++
			}
			if liveNames[r.Object.Name] {
				n++
			}
			objs = append(objs, &lcObject{rec: r, newer: n, noncur: parseTS(r.Object.TimeDeleted)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	now := s.now()
	for _, o := range objs {
		var del bool
		class := ""
		for _, rule := range b.Bucket.Lifecycle.Rule {
			if rule.Condition == nil || rule.Action == nil || !matches(rule.Condition, o, now) {
				continue
			}
			switch rule.Action.Type {
			case "Delete":
				del = true
			case "SetStorageClass":
				class = strings.ToUpper(rule.Action.StorageClass)
			}
		}
		obj := o.rec.Object
		switch {
		case del:
			gen := int64(0)
			if !o.live {
				gen = obj.Generation
			}
			if err := s.removeObject(ctx, name, obj.Name, gen, conds{GenMatch: &obj.Generation}, ""); err != nil {
				s.log.Debug("lifecycle delete skipped", "bucket", name, "object", obj.Name, "err", err)
			}
		case class != "" && class != obj.StorageClass:
			s.setStorageClass(name, o, class, now)
		}
	}
	if err := s.abortStaleMultipart(b, now); err != nil {
		return err
	}
	return nil
}

// setStorageClass changes an object's storage class in place (lifecycle
// SetStorageClass).
func (s *Service) setStorageClass(bucket string, o *lcObject, class string, now time.Time) {
	_ = s.env.Store.Update(func(tx store.Tx) error {
		rec, live, err := loadObject(tx, bucket, o.rec.Object.Name, o.rec.Object.Generation)
		if err != nil || rec == nil {
			return err
		}
		rec.Object.StorageClass = class
		rec.Object.TimeStorageClassUpdated = ts(now)
		rec.Object.Updated = ts(now)
		if live {
			return store.PutJSON(tx, nsObjects, objKey(bucket, rec.Object.Name), rec)
		}
		return store.PutJSON(tx, nsVersions, verKey(bucket, rec.Object.Name, rec.Object.Generation), rec)
	})
}

// abortStaleMultipart applies AbortIncompleteMultipartUpload rules to XML
// API multipart uploads.
func (s *Service) abortStaleMultipart(b *bucketRec, now time.Time) error {
	var maxAge *int64
	for _, rule := range b.Bucket.Lifecycle.Rule {
		if rule.Action != nil && rule.Action.Type == "AbortIncompleteMultipartUpload" && rule.Condition != nil && rule.Condition.Age != nil {
			if maxAge == nil || *rule.Condition.Age < *maxAge {
				maxAge = rule.Condition.Age
			}
		}
	}
	if maxAge == nil {
		return nil
	}
	var stale []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsMPU, "", func(k string, v []byte) bool {
			var m mpuRec
			if jsonUnmarshal(v, &m) == nil && m.Bucket == b.Bucket.Name && now.Sub(parseTS(m.Created)) >= time.Duration(*maxAge)*day {
				stale = append(stale, k)
			}
			return true
		})
		return nil
	})
	for _, id := range stale {
		s.abortMultipart(id)
	}
	return nil
}
