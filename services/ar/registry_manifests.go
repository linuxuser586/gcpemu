package ar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Manifest media types (Docker v2 schema 2 and OCI image spec v1.1).
const (
	mtDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mtDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mtOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mtDockerConfig   = "application/vnd.docker.container.image.v1+json"
	mtOCIConfig      = "application/vnd.oci.image.config.v1+json"
	mtOCIEmpty       = "application/vnd.oci.empty.v1+json"
)

// maxManifest is the largest manifest accepted (as the real registry).
const maxManifest = 4 << 20

func isIndex(mt string) bool { return mt == mtDockerList || mt == mtOCIIndex }

func isManifestType(mt string) bool {
	switch mt {
	case mtDockerManifest, mtDockerList, mtOCIManifest, mtOCIIndex:
		return true
	}
	return false
}

// nonDistributable reports layers whose bytes are not pushed to registries.
func nonDistributable(d descriptor) bool {
	return len(d.URLs) > 0 ||
		strings.Contains(d.MediaType, "foreign") ||
		strings.Contains(d.MediaType, "nondistributable")
}

type platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	URLs         []string          `json:"urls,omitempty"`
	Platform     *platform         `json:"platform,omitempty"`
}

type manifestDoc struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *descriptor       `json:"config,omitempty"`
	Layers        []descriptor      `json:"layers,omitempty"`
	Manifests     []descriptor      `json:"manifests,omitempty"`
	Subject       *descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// manifests serves /v2/<name>/manifests/<reference>.
func (s *Service) manifests(w http.ResponseWriter, r *http.Request, t *target, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !s.authorize(w, r, t, permDownload) {
			return
		}
		s.getManifest(w, r, t, ref)
	case http.MethodPut:
		if !s.authorize(w, r, t, permUpload) {
			return
		}
		s.putManifest(w, r, t, ref)
	case http.MethodDelete:
		if !s.authorize(w, r, t, permDelete) {
			return
		}
		s.deleteManifest(w, t, ref)
	default:
		writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed"))
	}
}

func isDigestRef(ref string) bool { return strings.Contains(ref, ":") }

// lookupManifest resolves a tag or digest to a manifest record.
func (s *Service) lookupManifest(t *target, ref string) (*manifestRec, *regError) {
	var m *manifestRec
	_ = s.env.Store.View(func(tx store.Tx) error {
		digest := ref
		if !isDigestRef(ref) {
			tg, ok := getTag(tx, t.ref, t.img, ref)
			if !ok {
				return nil
			}
			digest = tg.Digest
		}
		m, _ = getManifest(tx, t.ref, t.img, digest)
		return nil
	})
	if m == nil {
		return nil, regErr(http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown: %s", ref)
	}
	return m, nil
}

// accepts implements content negotiation: an absent Accept header or a
// wildcard accepts anything; otherwise the stored type must be listed.
// Docker v2 schema 2 manifests are always served to clients that list no
// manifest types, as the reference registry does.
func accepts(r *http.Request, mt string) bool {
	vals := r.Header.Values("Accept")
	if len(vals) == 0 {
		return true
	}
	listsManifest := false
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			t, _, _ := mime.ParseMediaType(strings.TrimSpace(part))
			if t == mt || t == "*/*" {
				return true
			}
			if isManifestType(t) {
				listsManifest = true
			}
		}
	}
	return !listsManifest && mt == mtDockerManifest
}

func (s *Service) getManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) {
	if !isDigestRef(ref) && !tagRe.MatchString(ref) {
		writeRegError(w, regErr(http.StatusBadRequest, "TAG_INVALID", "invalid tag %q", ref))
		return
	}
	m, rerr := s.lookupManifest(t, ref)
	if rerr != nil {
		writeRegError(w, rerr)
		return
	}
	body, err := s.blobs.read(m.Digest)
	if err != nil {
		writeRegError(w, regErr(http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown: %s", ref))
		return
	}
	writeManifest(w, r, m, body)
}

// putManifest validates and stores a manifest (and tag).
func (s *Service) putManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifest+1))
	if err != nil {
		writeRegError(w, regErr(http.StatusBadRequest, "MANIFEST_INVALID", "read manifest: %v", err))
		return
	}
	if len(body) > maxManifest {
		writeRegError(w, regErr(http.StatusRequestEntityTooLarge, "SIZE_INVALID", "manifest exceeds %d bytes", maxManifest))
		return
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	tag := ""
	if isDigestRef(ref) {
		if ref != digest {
			writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "provided digest %s did not match manifest content %s", ref, digest))
			return
		}
	} else if !tagRe.MatchString(ref) {
		writeRegError(w, regErr(http.StatusBadRequest, "TAG_INVALID", "invalid tag %q", ref))
		return
	} else {
		tag = ref
	}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		writeRegError(w, regErr(http.StatusBadRequest, "MANIFEST_INVALID", "manifest invalid: %v", err))
		return
	}
	mt := manifestType(r.Header.Get("Content-Type"), &doc)
	if !isManifestType(mt) {
		writeRegError(w, regErr(http.StatusBadRequest, "MANIFEST_INVALID", "unsupported manifest media type %q", mt))
		return
	}
	if doc.MediaType != "" && doc.MediaType != mt {
		writeRegError(w, regErr(http.StatusBadRequest, "MANIFEST_INVALID", "mediaType %q in manifest does not match Content-Type %q", doc.MediaType, mt))
		return
	}
	if doc.SchemaVersion != 2 {
		writeRegError(w, regErr(http.StatusBadRequest, "MANIFEST_INVALID", "unsupported schemaVersion %d", doc.SchemaVersion))
		return
	}
	now := s.env.Clock.Now()
	rec := &manifestRec{
		Digest: digest, MediaType: mt, Size: int64(len(body)), ArtifactType: doc.ArtifactType,
		Annotations: doc.Annotations, ContentSize: int64(len(body)), Created: now, Updated: now,
	}
	if doc.Subject != nil {
		rec.Subject = doc.Subject.Digest
	}
	if rerr := s.checkReferences(t, mt, &doc, rec); rerr != nil {
		writeRegError(w, rerr)
		return
	}

	s.blobs.gc.RLock()
	_, _, err = s.blobs.put(bytes.NewReader(body), digest)
	if err == nil {
		err = s.env.Store.Update(func(tx store.Tx) error {
			repo, err := getRepo(tx, t.ref)
			if err != nil {
				return err
			}
			if old, ok := getManifest(tx, t.ref, t.img, digest); ok {
				rec.Created = old.Created
			}
			if tag != "" {
				old, exists := getTag(tx, t.ref, t.img, tag)
				if exists && old.Digest != digest && repo.GetDockerConfig().GetImmutableTags() {
					return errImmutableTag
				}
				tr := tagRec{Digest: digest, Created: now, Updated: now}
				if exists {
					tr.Created = old.Created
				}
				if err := store.PutJSON(tx, nsTags, itemKey(t.ref, t.img, tag), tr); err != nil {
					return err
				}
			}
			return putManifest(tx, t.ref, t.img, rec)
		})
	}
	s.blobs.gc.RUnlock()
	switch {
	case errors.Is(err, errImmutableTag):
		writeRegError(w, regErr(http.StatusBadRequest, "TAG_INVALID", "tag %q is immutable in repository %s", tag, t.ref.name()))
		return
	case err != nil && apierr.From(err).HTTP() == http.StatusNotFound:
		writeRegError(w, errNameUnknown(t.name))
		return
	case err != nil:
		writeRegError(w, regErr(http.StatusInternalServerError, "UNKNOWN", "%v", err))
		return
	}
	h := w.Header()
	h.Set("Location", t.path("/manifests/"+digest))
	h.Set("Docker-Content-Digest", digest)
	if rec.Subject != "" {
		h.Set("OCI-Subject", rec.Subject)
	}
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

var errImmutableTag = errors.New("immutable tag")

// manifestType determines the manifest media type from Content-Type or,
// failing that, the document itself.
func manifestType(ct string, doc *manifestDoc) string {
	if ct != "" {
		if t, _, err := mime.ParseMediaType(ct); err == nil && t != "application/json" && t != "application/octet-stream" {
			return t
		}
	}
	switch {
	case doc.MediaType != "":
		return doc.MediaType
	case doc.Manifests != nil:
		return mtOCIIndex
	default:
		return mtOCIManifest
	}
}

// checkReferences verifies every blob or child manifest the manifest
// references exists in the repository and fills sizes and platform data.
func (s *Service) checkReferences(t *target, mt string, doc *manifestDoc, rec *manifestRec) *regError {
	var rerr *regError
	_ = s.env.Store.View(func(tx store.Tx) error {
		if isIndex(mt) {
			for _, d := range doc.Manifests {
				if _, err := parseDigest(d.Digest); err != nil {
					rerr = regErr(http.StatusBadRequest, "MANIFEST_INVALID", "invalid manifest digest %q", d.Digest)
					return nil
				}
				if _, ok := getManifest(tx, t.ref, t.img, d.Digest); !ok {
					rerr = regErr(http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "manifest %s referenced by index is unknown to the repository", d.Digest)
					rerr.detail = map[string]string{"digest": d.Digest}
					return nil
				}
				c := childRec{Digest: d.Digest, MediaType: d.MediaType, Size: d.Size}
				if p := d.Platform; p != nil {
					c.OS, c.Architecture, c.Variant, c.OSVersion, c.OSFeatures = p.OS, p.Architecture, p.Variant, p.OSVersion, p.OSFeatures
				}
				rec.Children = append(rec.Children, c)
				rec.ContentSize += d.Size
			}
			return nil
		}
		if doc.Config == nil {
			rerr = regErr(http.StatusBadRequest, "MANIFEST_INVALID", "manifest has no config descriptor")
			return nil
		}
		if rec.ArtifactType == "" && doc.Config.MediaType != mtDockerConfig && doc.Config.MediaType != mtOCIConfig && doc.Config.MediaType != mtOCIEmpty {
			rec.ArtifactType = doc.Config.MediaType
		}
		for _, d := range append([]descriptor{*doc.Config}, doc.Layers...) {
			if nonDistributable(d) {
				continue
			}
			if _, err := parseDigest(d.Digest); err != nil {
				rerr = regErr(http.StatusBadRequest, "MANIFEST_INVALID", "invalid blob digest %q", d.Digest)
				return nil
			}
			l, ok := getLink(tx, t.ref, t.img, d.Digest)
			if !ok {
				rerr = regErr(http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "blob unknown to registry: %s", d.Digest)
				rerr.detail = map[string]string{"digest": d.Digest}
				return nil
			}
			if d.Size != l.Size {
				rerr = regErr(http.StatusBadRequest, "MANIFEST_INVALID", "size of blob %s is %d, manifest declares %d", d.Digest, l.Size, d.Size)
				return nil
			}
			rec.Blobs = append(rec.Blobs, d.Digest)
			rec.ContentSize += d.Size
		}
		return nil
	})
	if rerr == nil && !isIndex(mt) && (doc.Config.MediaType == mtDockerConfig || doc.Config.MediaType == mtOCIConfig) {
		rec.BuildTime = s.buildTime(doc.Config.Digest)
	}
	return rerr
}

// buildTime reads "created" from an image config blob.
func (s *Service) buildTime(digest string) *time.Time {
	b, err := s.blobs.read(digest)
	if err != nil || len(b) > maxManifest {
		return nil
	}
	var cfg struct {
		Created *time.Time `json:"created"`
	}
	if json.Unmarshal(b, &cfg) != nil || cfg.Created == nil || cfg.Created.IsZero() {
		return nil
	}
	t := cfg.Created.UTC()
	return &t
}

// deleteManifest deletes a manifest by digest (with its tags) or a tag.
func (s *Service) deleteManifest(w http.ResponseWriter, t *target, ref string) {
	var missing bool
	var removed bool
	err := s.env.Store.Update(func(tx store.Tx) error {
		if !isDigestRef(ref) {
			if _, ok := getTag(tx, t.ref, t.img, ref); !ok {
				missing = true
				return nil
			}
			return tx.Delete(nsTags, itemKey(t.ref, t.img, ref))
		}
		if _, ok := getManifest(tx, t.ref, t.img, ref); !ok {
			missing = true
			return nil
		}
		removed = true
		return deleteManifest(tx, t.ref, t.img, ref)
	})
	if err != nil {
		writeRegError(w, regErr(http.StatusInternalServerError, "UNKNOWN", "%v", err))
		return
	}
	if missing {
		writeRegError(w, regErr(http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown: %s", ref))
		return
	}
	if removed {
		s.collect([]string{ref})
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}

// referrers implements GET /v2/<name>/referrers/<digest> (OCI 1.1).
func (s *Service) referrers(w http.ResponseWriter, r *http.Request, t *target, digest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed"))
		return
	}
	if _, err := parseDigest(digest); err != nil {
		writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "%v", err))
		return
	}
	if !s.authorize(w, r, t, permDownload) {
		return
	}
	filter := r.URL.Query().Get("artifactType")
	out := []descriptor{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		_, ms := listManifests(tx, t.ref, t.img)
		for _, m := range ms {
			if m.Subject != digest || (filter != "" && m.ArtifactType != filter) {
				continue
			}
			out = append(out, descriptor{MediaType: m.MediaType, Digest: m.Digest, Size: m.Size, ArtifactType: m.ArtifactType, Annotations: m.Annotations})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	if filter != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	b, _ := json.Marshal(struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Manifests     []descriptor `json:"manifests"`
	}{2, mtOCIIndex, out})
	w.Header().Set("Content-Type", mtOCIIndex)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}
