package gcs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// HMAC keys (FR-GCS-005): projects.hmacKeys in the JSON API; the secrets
// authenticate XML API requests and V4 signatures.

// hmacRec is a stored HMAC key.
type hmacRec struct {
	Meta   *storage.HmacKeyMetadata `json:"meta"`
	Secret string                   `json:"secret"`
	Seq    int64                    `json:"seq"`
}

const accessIDChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func newAccessID() string {
	b := make([]byte, 61-len("GOOG1E"))
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = accessIDChars[int(b[i])%len(accessIDChars)]
	}
	return "GOOG1E" + string(b)
}

func newSecret() string {
	b := make([]byte, 30)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func renderHMAC(rec *hmacRec, base string) *storage.HmacKeyMetadata {
	m := *rec.Meta
	m.SelfLink = base + "/storage/v1/projects/" + m.ProjectId + "/hmacKeys/" + m.AccessId
	m.Etag = bucketEtag(rec.Seq)
	return &m
}

// hmacSecret looks up an active HMAC key by access ID.
func (s *Service) hmacSecret(_ context.Context, accessID string) (*hmacRec, error) {
	var rec hmacRec
	err := s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsHMAC, accessID, &rec) })
	if err != nil {
		return nil, &sigError{http.StatusForbidden, "InvalidAccessKeyId", "The Google Cloud Storage access key Id you provided does not exist in our records.", ""}
	}
	if rec.Meta.State != "ACTIVE" {
		return nil, &sigError{http.StatusForbidden, "InvalidAccessKeyId", "The Google Cloud Storage access key Id you provided is not active.", ""}
	}
	return &rec, nil
}

// serveHMAC routes /storage/v1/projects/P/hmacKeys[/ID].
func (s *Service) serveHMAC(w http.ResponseWriter, r *http.Request, proj string, rest []string) {
	ctx := r.Context()
	if err := s.env.EnsureProject(proj); err != nil {
		apierr.Write(w, err)
		return
	}
	res := projectResource(proj)
	base := baseURL(r)
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		if err := s.check(ctx, "storage.hmacKeys.create", res); err != nil {
			apierr.Write(w, err)
			return
		}
		email := r.URL.Query().Get("serviceAccountEmail")
		if email == "" {
			apierr.Write(w, errRequired("serviceAccountEmail"))
			return
		}
		rec, err := s.createHMAC(proj, email)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		writeJSON(w, http.StatusOK, &storage.HmacKey{Kind: "storage#hmacKey", Metadata: renderHMAC(rec, base), Secret: rec.Secret})
	case len(rest) == 0 && r.Method == http.MethodGet:
		if err := s.check(ctx, "storage.hmacKeys.list", res); err != nil {
			apierr.Write(w, err)
			return
		}
		q := r.URL.Query()
		email := q.Get("serviceAccountEmail")
		showDeleted := q.Get("showDeletedKeys") == "true"
		out := &storage.HmacKeysMetadata{Kind: "storage#hmacKeysMetadata"}
		_ = s.env.Store.View(func(tx store.Tx) error {
			all, err := store.ListJSON[*hmacRec](tx, nsHMAC, "")
			for _, k := range all {
				if k.Meta.ProjectId != proj || (email != "" && k.Meta.ServiceAccountEmail != email) || (!showDeleted && k.Meta.State == "DELETED") {
					continue
				}
				out.Items = append(out.Items, renderHMAC(k, base))
			}
			return err
		})
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 1:
		s.serveHMACKey(w, r, proj, rest[0])
	default:
		methodNotAllowed(w, r)
	}
}

func (s *Service) createHMAC(proj, email string) (*hmacRec, error) {
	now := ts(s.now())
	rec := &hmacRec{
		Meta: &storage.HmacKeyMetadata{
			Kind: "storage#hmacKeyMetadata", AccessId: newAccessID(), ProjectId: proj,
			ServiceAccountEmail: email, State: "ACTIVE", TimeCreated: now, Updated: now,
		},
		Secret: newSecret(),
		Seq:    1,
	}
	rec.Meta.Id = proj + "/" + rec.Meta.AccessId
	err := s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsHMAC, rec.Meta.AccessId, rec) })
	return rec, err
}

func (s *Service) serveHMACKey(w http.ResponseWriter, r *http.Request, proj, id string) {
	ctx := r.Context()
	perm := map[string]string{
		http.MethodGet: "storage.hmacKeys.get", http.MethodPut: "storage.hmacKeys.update", http.MethodDelete: "storage.hmacKeys.delete",
	}[r.Method]
	if perm == "" {
		methodNotAllowed(w, r)
		return
	}
	if err := s.check(ctx, perm, projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	var upd storage.HmacKeyMetadata
	if r.Method == http.MethodPut {
		if err := decodeJSON(r, &upd); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	var out *hmacRec
	err := s.env.Store.Update(func(tx store.Tx) error {
		var rec hmacRec
		if err := store.GetJSON(tx, nsHMAC, id, &rec); err != nil || rec.Meta.ProjectId != proj {
			return apierr.NotFound("Access ID not found in project %s.", proj)
		}
		out = &rec
		switch r.Method {
		case http.MethodGet:
			return nil
		case http.MethodPut:
			if upd.Etag != "" && upd.Etag != bucketEtag(rec.Seq) {
				return errPrecondition()
			}
			st := strings.ToUpper(upd.State)
			if st != "ACTIVE" && st != "INACTIVE" {
				return errInvalid("Invalid state: %s. State must be ACTIVE or INACTIVE.", upd.State)
			}
			if rec.Meta.State == "DELETED" {
				return errInvalid("Cannot update a deleted key.")
			}
			rec.Meta.State = st
			rec.Meta.Updated = ts(s.now())
			rec.Seq++
			return store.PutJSON(tx, nsHMAC, id, &rec)
		default: // DELETE
			if rec.Meta.State != "INACTIVE" {
				return errInvalid("Cannot delete keys in 'ACTIVE' state.")
			}
			return tx.Delete(nsHMAC, id)
		}
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, renderHMAC(out, baseURL(r)))
}
