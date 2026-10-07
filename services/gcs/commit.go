package gcs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
)

var jsonUnmarshal = json.Unmarshal

// loadObject returns the live object, or the given generation (live or
// noncurrent) when gen != 0. It returns nil, nil when absent.
func loadObject(tx store.Tx, bucket, name string, gen int64) (*objectRec, bool, error) {
	var rec objectRec
	err := store.GetJSON(tx, nsObjects, objKey(bucket, name), &rec)
	if err != nil && err != store.ErrNotFound {
		return nil, false, err
	}
	if err == nil && (gen == 0 || rec.Object.Generation == gen) {
		return &rec, true, nil
	}
	if gen == 0 {
		return nil, false, nil
	}
	var v objectRec
	if err := store.GetJSON(tx, nsVersions, verKey(bucket, name, gen), &v); err != nil {
		if err == store.ErrNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &v, false, nil
}

// maxGeneration returns the highest generation ever stored for a name.
func maxGeneration(tx store.Tx, bucket, name string, live *objectRec) int64 {
	var max int64
	if live != nil {
		max = live.Object.Generation
	}
	tx.Scan(nsVersions, verPrefix(bucket, name), func(_ string, v []byte) bool {
		var rec objectRec
		if jsonUnmarshal(v, &rec) == nil && rec.Object.Generation > max {
			max = rec.Object.Generation
		}
		return true
	})
	return max
}

// newGeneration returns a microsecond-timestamp generation greater than
// every earlier generation of the object.
func newGeneration(now time.Time, prevMax int64) int64 {
	g := now.UnixMicro()
	if g <= prevMax {
		g = prevMax + 1
	}
	return g
}

// lockedErr reports whether holds or retention prevent deleting, overwriting
// or archiving rec (FR-GCS-001 retention policy, event-based/temporary holds).
func lockedErr(rec *objectRec, bkt *storage.Bucket, now time.Time) error {
	o := rec.Object
	if o.EventBasedHold {
		return errForbidden("Object '%s/%s' is under active Event-Based hold and cannot be deleted, overwritten or archived until hold is removed.", o.Bucket, o.Name)
	}
	if o.TemporaryHold {
		return errForbidden("Object '%s/%s' is under active Temporary hold and cannot be deleted, overwritten or archived until hold is removed.", o.Bucket, o.Name)
	}
	if t, ok := retentionExpiry(rec, bkt); ok && now.Before(t) {
		return errForbidden("Object '%s/%s' is subject to bucket's retention policy or object retention and cannot be deleted or overwritten until %s", o.Bucket, o.Name, ts(t))
	}
	if o.Retention != nil && o.Retention.RetainUntilTime != "" {
		if t := parseTS(o.Retention.RetainUntilTime); now.Before(t) {
			return errForbidden("Object '%s/%s' is subject to bucket's retention policy or object retention and cannot be deleted or overwritten until %s", o.Bucket, o.Name, ts(t))
		}
	}
	return nil
}

// writeReq is a request to create a new object generation from a blob.
type writeReq struct {
	bucket string
	// meta carries the user-settable metadata (name, contentType, ...).
	meta  *storage.Object
	blob  *blobInfo
	conds conds
	// componentCount > 0 marks a composite object (no MD5).
	componentCount int64
	// base is the URL base used for notification payloads.
	base string
	// extra lets compose delete its sources in the same transaction.
	extra func(tx store.Tx, bkt *bucketRec, now time.Time) ([]event, []string, error)
}

// commitObject atomically installs a new generation (FR-GCS-002/003): it
// checks preconditions, holds and retention, archives or deletes the
// previous live generation, and publishes notifications after the commit.
// The blob is removed if the commit fails.
func (s *Service) commitObject(ctx context.Context, wr *writeReq) (*objectRec, *storage.Bucket, error) {
	var (
		out      *objectRec
		bkt      *storage.Bucket
		events   []event
		oldBlobs []string
	)
	err := s.env.Store.Update(func(tx store.Tx) error {
		events, oldBlobs = nil, nil
		brec, err := loadBucket(tx, wr.bucket)
		if err != nil {
			return err
		}
		bkt = brec.Bucket
		cur, _, err := loadObject(tx, wr.bucket, wr.meta.Name, 0)
		if err != nil {
			return err
		}
		var curObj *storage.Object
		if cur != nil {
			curObj = cur.Object
		}
		if err := wr.conds.check(curObj, false); err != nil {
			return err
		}
		now := s.now()
		if cur != nil {
			if err := lockedErr(cur, bkt, now); err != nil {
				return err
			}
		}
		gen := newGeneration(now, maxGeneration(tx, wr.bucket, wr.meta.Name, cur))
		obj := buildObject(wr, bkt, gen, now)
		rec := &objectRec{Object: obj, Blob: wr.blob.ID, RetentionStart: obj.TimeCreated}
		if wr.extra != nil {
			ev, blobs, err := wr.extra(tx, brec, now)
			if err != nil {
				return err
			}
			events = append(events, ev...)
			oldBlobs = append(oldBlobs, blobs...)
		}
		if cur != nil {
			ev, blob, err := retire(tx, cur, bkt, now, gen)
			if err != nil {
				return err
			}
			events = append(events, ev)
			if blob != "" {
				oldBlobs = append(oldBlobs, blob)
			}
		}
		fin := event{typ: evFinalize, rec: rec}
		if cur != nil {
			fin.attrs = map[string]string{"overwroteGeneration": fmt.Sprint(cur.Object.Generation)}
		}
		events = append(events, fin)
		out = rec
		return store.PutJSON(tx, nsObjects, objKey(wr.bucket, obj.Name), rec)
	})
	if err != nil {
		s.blobs.remove(wr.blob.ID)
		return nil, nil, err
	}
	for _, b := range oldBlobs {
		s.blobs.remove(b)
	}
	s.publish(ctx, wr.bucket, bkt, wr.base, events)
	return out, bkt, nil
}

// retire moves the live generation cur out of the way of a new generation
// (overwrittenBy) or a delete (overwrittenBy == 0): with versioning on it
// becomes noncurrent (OBJECT_ARCHIVE); otherwise it is deleted
// (OBJECT_DELETE) and its blob ID is returned for removal after commit.
func retire(tx store.Tx, cur *objectRec, bkt *storage.Bucket, now time.Time, overwrittenBy int64) (event, string, error) {
	o := cur.Object
	if err := tx.Delete(nsObjects, objKey(o.Bucket, o.Name)); err != nil {
		return event{}, "", err
	}
	ev := event{rec: cur}
	if overwrittenBy != 0 {
		ev.attrs = map[string]string{"overwrittenByGeneration": fmt.Sprint(overwrittenBy)}
	}
	if bkt.Versioning != nil && bkt.Versioning.Enabled {
		o.TimeDeleted = ts(now)
		ev.typ = evArchive
		return ev, "", store.PutJSON(tx, nsVersions, verKey(o.Bucket, o.Name, o.Generation), cur)
	}
	ev.typ = evDelete
	return ev, cur.Blob, nil
}

// buildObject creates the stored metadata of a new generation.
func buildObject(wr *writeReq, bkt *storage.Bucket, gen int64, now time.Time) *storage.Object {
	m := wr.meta
	o := &storage.Object{
		Kind:               "storage#object",
		Id:                 fmt.Sprintf("%s/%s/%d", wr.bucket, m.Name, gen),
		Name:               m.Name,
		Bucket:             wr.bucket,
		Generation:         gen,
		Metageneration:     1,
		ContentType:        m.ContentType,
		CacheControl:       m.CacheControl,
		ContentDisposition: m.ContentDisposition,
		ContentEncoding:    m.ContentEncoding,
		ContentLanguage:    m.ContentLanguage,
		CustomTime:         m.CustomTime,
		Metadata:           m.Metadata,
		Contexts:           m.Contexts,
		EventBasedHold:     m.EventBasedHold || bkt.DefaultEventBasedHold,
		TemporaryHold:      m.TemporaryHold,
		Retention:          m.Retention,
		StorageClass:       strings.ToUpper(m.StorageClass),
		Size:               uint64(wr.blob.Size),
		Crc32c:             wr.blob.crcB64(),
		ComponentCount:     wr.componentCount,
		TimeCreated:        ts(now),
		Updated:            ts(now),
		TimeFinalized:      ts(now),
	}
	o.TimeStorageClassUpdated = o.TimeCreated
	if o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	if o.StorageClass == "" {
		o.StorageClass = bkt.StorageClass
	}
	if wr.componentCount == 0 {
		o.Md5Hash = wr.blob.md5B64()
	}
	return o
}
