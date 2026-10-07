package gcs

import (
	"context"
	"net/http"
	"strconv"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Object metadata operations (FR-GCS-002, FR-GCS-003).

// mutableObjectFields are the object fields patch/update may change.
var mutableObjectFields = map[string]bool{
	"cacheControl": true, "contentDisposition": true, "contentEncoding": true, "contentLanguage": true,
	"contentType": true, "customTime": true, "eventBasedHold": true, "metadata": true,
	"temporaryHold": true, "retention": true, "contexts": true,
}

// parseGeneration reads the "generation" query parameter (0 = live).
func parseGeneration(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	g, err := strconv.ParseInt(v, 10, 64)
	if err != nil || g < 0 {
		return 0, errInvalid("Invalid argument for generation: %q", v)
	}
	return g, nil
}

// readObject loads an object (and its bucket) for a read, evaluating
// read preconditions.
func (s *Service) readObject(bucket, name string, gen int64, c conds) (*objectRec, *storage.Bucket, error) {
	var (
		rec *objectRec
		bkt *storage.Bucket
	)
	err := s.env.Store.View(func(tx store.Tx) error {
		brec, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		bkt = brec.Bucket
		rec, _, err = loadObject(tx, bucket, name, gen)
		if err != nil {
			return err
		}
		if rec == nil {
			return errObjectNotFound(bucket, name)
		}
		return c.check(rec.Object, true)
	})
	return rec, bkt, err
}

func (s *Service) getObject(w http.ResponseWriter, r *http.Request, bucket, name string) {
	q := r.URL.Query()
	if q.Get("alt") == "media" {
		s.downloadJSON(w, r, bucket, name)
		return
	}
	if err := s.check(r.Context(), "storage.objects.get", objectResource(bucket, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	gen, err := parseGeneration(q.Get("generation"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(q, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	rec, bkt, err := s.readObject(bucket, name, gen, c)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderObject(rec, bkt, baseURL(r)))
}

func (s *Service) patchObject(w http.ResponseWriter, r *http.Request, bucket, name string, replace bool) {
	if err := s.check(r.Context(), "storage.objects.update", objectResource(bucket, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	q := r.URL.Query()
	gen, err := parseGeneration(q.Get("generation"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(q, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	body, _, err := readJSONBody(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	rec, bkt, err := s.updateObjectMeta(r.Context(), bucket, name, gen, c, baseURL(r), func(o *storage.Object) (*storage.Object, error) {
		var no storage.Object
		if err := applyPatch(o, body, mutableObjectFields, replace, &no); err != nil {
			return nil, err
		}
		return &no, nil
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderObject(rec, bkt, baseURL(r)))
}

// updateObjectMeta applies fn to an object's metadata, bumping its
// metageneration and publishing OBJECT_METADATA_UPDATE.
func (s *Service) updateObjectMeta(ctx context.Context, bucket, name string, gen int64, c conds, base string, fn func(*storage.Object) (*storage.Object, error)) (*objectRec, *storage.Bucket, error) {
	var (
		out *objectRec
		bkt *storage.Bucket
	)
	err := s.env.Store.Update(func(tx store.Tx) error {
		brec, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		bkt = brec.Bucket
		rec, live, err := loadObject(tx, bucket, name, gen)
		if err != nil {
			return err
		}
		if rec == nil {
			return errObjectNotFound(bucket, name)
		}
		if err := c.check(rec.Object, false); err != nil {
			return err
		}
		prev := rec.Object
		no, err := fn(prev)
		if err != nil {
			return err
		}
		now := s.now()
		if prev.CustomTime != "" && no.CustomTime != prev.CustomTime {
			if no.CustomTime == "" || parseTS(no.CustomTime).Before(parseTS(prev.CustomTime)) {
				return errInvalid("Custom time cannot be removed or decreased.")
			}
		}
		if prev.EventBasedHold && !no.EventBasedHold {
			rec.RetentionStart = ts(now)
		}
		if no.ContentType == "" {
			no.ContentType = prev.ContentType
		}
		no.Metageneration = prev.Metageneration + 1
		no.Updated = ts(now)
		no.Acl = nil
		rec.Object = no
		out = rec
		if live {
			return store.PutJSON(tx, nsObjects, objKey(bucket, name), rec)
		}
		return store.PutJSON(tx, nsVersions, verKey(bucket, name, no.Generation), rec)
	})
	if err != nil {
		return nil, nil, err
	}
	s.publish(ctx, bucket, bkt, base, []event{{typ: evMetadataUpdate, rec: out}})
	return out, bkt, nil
}

func (s *Service) deleteObject(w http.ResponseWriter, r *http.Request, bucket, name string) {
	if err := s.check(r.Context(), "storage.objects.delete", objectResource(bucket, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	q := r.URL.Query()
	gen, err := parseGeneration(q.Get("generation"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	c, err := parseConds(q, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.removeObject(r.Context(), bucket, name, gen, c, baseURL(r)); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeObject deletes an object. Without a generation the live version is
// archived (versioning on) or deleted; with one, that generation is
// permanently deleted.
func (s *Service) removeObject(ctx context.Context, bucket, name string, gen int64, c conds, base string) error {
	var (
		ev   event
		blob string
		bkt  *storage.Bucket
	)
	err := s.env.Store.Update(func(tx store.Tx) error {
		brec, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		bkt = brec.Bucket
		rec, live, err := loadObject(tx, bucket, name, gen)
		if err != nil {
			return err
		}
		if rec == nil {
			return errObjectNotFound(bucket, name)
		}
		if err := c.check(rec.Object, false); err != nil {
			return err
		}
		now := s.now()
		if err := lockedErr(rec, bkt, now); err != nil {
			return err
		}
		if live && gen == 0 {
			ev, blob, err = retire(tx, rec, bkt, now, 0)
			return err
		}
		ev, blob = event{typ: evDelete, rec: rec}, rec.Blob
		if live {
			return tx.Delete(nsObjects, objKey(bucket, name))
		}
		return tx.Delete(nsVersions, verKey(bucket, name, rec.Object.Generation))
	})
	if err != nil {
		return err
	}
	s.blobs.remove(blob)
	s.publish(ctx, bucket, bkt, base, []event{ev})
	return nil
}
