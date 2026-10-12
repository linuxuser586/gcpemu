package gcs

import (
	storage "google.golang.org/api/storage/v1"
)

// Object Retention Lock (FR-GCS-001): an object's own retain-until time,
// in Unlocked mode (may be shortened or removed with
// overrideUnlockedRetention) or Locked mode (may only be extended).

const (
	retentionUnlocked = "Unlocked"
	retentionLocked   = "Locked"
)

// hasRetention reports whether r sets a retention configuration.
func hasRetention(r *storage.ObjectRetention) bool {
	return r != nil && (r.Mode != "" || r.RetainUntilTime != "")
}

// checkRetention validates a new or changed object retention
// configuration against the bucket and the object's previous one (nil for
// a new object). override is the request's overrideUnlockedRetention.
func checkRetention(prev, next *storage.ObjectRetention, bkt *storage.Bucket, override bool) error {
	if !hasRetention(prev) && !hasRetention(next) {
		return nil
	}
	if hasRetention(next) {
		if bkt.ObjectRetention == nil || bkt.ObjectRetention.Mode != "Enabled" {
			return errInvalid("Object retention is not enabled for bucket '%s'.", bkt.Name)
		}
		if next.Mode != retentionUnlocked && next.Mode != retentionLocked {
			return errInvalid("Invalid argument for retention.mode: %q", next.Mode)
		}
		if next.RetainUntilTime == "" || parseTS(next.RetainUntilTime).IsZero() {
			return errInvalid("Invalid argument for retention.retainUntilTime: %q", next.RetainUntilTime)
		}
	}
	if !hasRetention(prev) || !reducesRetention(prev, next) {
		return nil
	}
	if prev.Mode == retentionLocked {
		return errForbidden("Object retention in Locked mode cannot be removed, unlocked or shortened.")
	}
	if !override {
		return errForbidden("Removing or shortening Unlocked object retention requires overrideUnlockedRetention=true.")
	}
	return nil
}

// reducesRetention reports whether next removes, unlocks or shortens prev.
func reducesRetention(prev, next *storage.ObjectRetention) bool {
	if !hasRetention(next) {
		return true
	}
	if prev.Mode == retentionLocked && next.Mode != retentionLocked {
		return true
	}
	return parseTS(next.RetainUntilTime).Before(parseTS(prev.RetainUntilTime))
}
