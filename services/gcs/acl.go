package gcs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Legacy ACLs (FR-GCS-009): bucket, default object and object access
// controls. They are stored and returned but grant nothing; IAM alone
// decides access. Uniform bucket-level access hides them and refuses
// changes, as GCS does.

// aclEntry is one role/entity pair. Records keep ACLs in this form, apart
// from the stored storage.Bucket/Object, and render the API types from it.
type aclEntry struct {
	Entity string `json:"entity"`
	Role   string `json:"role"`
}

const ublaDoc = " Read more at https://cloud.google.com/storage/docs/uniform-bucket-level-access"

func ublaEnabled(b *storage.Bucket) bool {
	ic := b.IamConfiguration
	return ic != nil && ic.UniformBucketLevelAccess != nil && ic.UniformBucketLevelAccess.Enabled
}

// projectEntity is the entity of a project team ("owners", "editors",
// "viewers").
func projectEntity(team string, projNum uint64) string {
	return "project-" + team + "-" + strconv.FormatUint(projNum, 10)
}

// ownerEntity is the ACL entity of the principal creating an object.
func ownerEntity(p emu.Principal) string {
	if e := p.Email(); e != "" {
		return "user-" + e
	}
	return ""
}

// projectPrivate is the ACL GCS gives new buckets, and the default object
// ACL it gives their objects: project owners and editors own, viewers read.
func projectPrivate(projNum uint64) []aclEntry {
	return []aclEntry{
		{projectEntity("owners", projNum), "OWNER"},
		{projectEntity("editors", projNum), "OWNER"},
		{projectEntity("viewers", projNum), "READER"},
	}
}

// predefinedBucketACL expands a bucket predefinedAcl.
func predefinedBucketACL(name string, projNum uint64) ([]aclEntry, error) {
	owners := aclEntry{projectEntity("owners", projNum), "OWNER"}
	switch name {
	case "private":
		return []aclEntry{owners}, nil
	case "projectPrivate":
		return projectPrivate(projNum), nil
	case "publicRead":
		return []aclEntry{owners, {"allUsers", "READER"}}, nil
	case "publicReadWrite":
		return []aclEntry{owners, {"allUsers", "WRITER"}}, nil
	case "authenticatedRead":
		return []aclEntry{owners, {"allAuthenticatedUsers", "READER"}}, nil
	}
	return nil, errInvalid("Invalid argument for predefinedAcl: %q", name)
}

// predefinedObjectACL expands an object predefinedAcl, or a bucket
// predefinedDefaultObjectAcl when owner is empty (the object owner is only
// known when an object is created).
func predefinedObjectACL(name string, projNum uint64, owner string) ([]aclEntry, error) {
	var out []aclEntry
	switch name {
	case "private":
	case "projectPrivate":
		out = projectPrivate(projNum)
	case "bucketOwnerRead":
		out = []aclEntry{{projectEntity("owners", projNum), "READER"}}
	case "bucketOwnerFullControl":
		out = []aclEntry{{projectEntity("owners", projNum), "OWNER"}}
	case "publicRead":
		out = []aclEntry{{"allUsers", "READER"}}
	case "authenticatedRead":
		out = []aclEntry{{"allAuthenticatedUsers", "READER"}}
	default:
		return nil, errInvalid("Invalid argument for predefinedAcl: %q", name)
	}
	return withOwner(out, owner), nil
}

// withOwner adds the object owner as OWNER unless it is already listed.
func withOwner(acl []aclEntry, owner string) []aclEntry {
	if owner == "" {
		return acl
	}
	for _, e := range acl {
		if e.Entity == owner {
			return acl
		}
	}
	return append(acl, aclEntry{owner, "OWNER"})
}

// validEntity reports whether entity names a grantee GCS accepts.
func validEntity(entity string) bool {
	switch entity {
	case "allUsers", "allAuthenticatedUsers":
		return true
	}
	for _, p := range []string{"user-", "group-", "domain-", "project-owners-", "project-editors-", "project-viewers-"} {
		if strings.HasPrefix(entity, p) && len(entity) > len(p) {
			return true
		}
	}
	return false
}

// checkEntry validates a role/entity pair. Objects have no WRITER role.
func checkEntry(e aclEntry, object bool) error {
	if e.Entity == "" {
		return errRequired("entity")
	}
	if !validEntity(e.Entity) {
		return errInvalid("Invalid value for entity: '%s'", e.Entity)
	}
	switch e.Role {
	case "OWNER", "READER":
		return nil
	case "WRITER":
		if !object {
			return nil
		}
	case "":
		return errRequired("role")
	}
	return errInvalid("Invalid value for role: '%s'", e.Role)
}

// entriesOf converts request ACL items, validating each.
func entriesOf[T any](items []T, object bool, pair func(T) aclEntry) ([]aclEntry, error) {
	out := make([]aclEntry, 0, len(items))
	for _, it := range items {
		e := pair(it)
		if err := checkEntry(e, object); err != nil {
			return nil, err
		}
		out = upsertEntry(out, e)
	}
	return out, nil
}

func bucketPair(a *storage.BucketAccessControl) aclEntry { return aclEntry{a.Entity, a.Role} }

func objectPair(a *storage.ObjectAccessControl) aclEntry { return aclEntry{a.Entity, a.Role} }

// upsertEntry sets entity's role, appending it when absent.
func upsertEntry(acl []aclEntry, e aclEntry) []aclEntry {
	for i := range acl {
		if acl[i].Entity == e.Entity {
			out := append([]aclEntry(nil), acl...)
			out[i] = e
			return out
		}
	}
	return append(append([]aclEntry(nil), acl...), e)
}

// entityInfo holds what GCS derives from an entity.
type entityInfo struct {
	email, id, domain string
	team              string
	projNum           string
}

func parseEntity(entity string) entityInfo {
	var in entityInfo
	switch {
	case strings.HasPrefix(entity, "user-"), strings.HasPrefix(entity, "group-"):
		_, v, _ := strings.Cut(entity, "-")
		if strings.Contains(v, "@") {
			in.email = v
		} else {
			in.id = v
		}
	case strings.HasPrefix(entity, "domain-"):
		in.domain = strings.TrimPrefix(entity, "domain-")
	case strings.HasPrefix(entity, "project-"):
		rest := strings.TrimPrefix(entity, "project-")
		team, p, _ := strings.Cut(rest, "-")
		in.team = team
		in.projNum = p
		if !isDigits(p) {
			in.projNum = project.NumberString(p)
		}
	}
	return in
}

// renderBucketACL renders a bucket's ACL.
func renderBucketACL(acl []aclEntry, b *storage.Bucket, base string) []*storage.BucketAccessControl {
	out := make([]*storage.BucketAccessControl, 0, len(acl))
	for _, e := range acl {
		in := parseEntity(e.Entity)
		a := &storage.BucketAccessControl{
			Kind:     "storage#bucketAccessControl",
			Id:       b.Name + "/" + e.Entity,
			SelfLink: base + "/storage/v1/b/" + b.Name + "/acl/" + url.PathEscape(e.Entity),
			Bucket:   b.Name,
			Entity:   e.Entity,
			Role:     e.Role,
			Email:    in.email,
			EntityId: in.id,
			Domain:   in.domain,
			Etag:     bucketEtag(b.Metageneration),
		}
		if in.team != "" {
			a.ProjectTeam = &storage.BucketAccessControlProjectTeam{ProjectNumber: in.projNum, Team: in.team}
		}
		out = append(out, a)
	}
	return out
}

// renderObjectACL renders an object's ACL, or a bucket's default object
// ACL when o is nil.
func renderObjectACL(acl []aclEntry, b *storage.Bucket, o *storage.Object, base string) []*storage.ObjectAccessControl {
	out := make([]*storage.ObjectAccessControl, 0, len(acl))
	for _, e := range acl {
		in := parseEntity(e.Entity)
		a := &storage.ObjectAccessControl{
			Kind:     "storage#objectAccessControl",
			Bucket:   b.Name,
			Entity:   e.Entity,
			Role:     e.Role,
			Email:    in.email,
			EntityId: in.id,
			Domain:   in.domain,
			Etag:     bucketEtag(b.Metageneration),
		}
		if o != nil {
			a.Object = o.Name
			a.Generation = o.Generation
			a.Id = o.Bucket + "/" + o.Name + "/" + strconv.FormatInt(o.Generation, 10) + "/" + e.Entity
			a.SelfLink = base + "/storage/v1/b/" + o.Bucket + "/o/" + url.PathEscape(o.Name) + "/acl/" + url.PathEscape(e.Entity)
			a.Etag = etagFor(o.Generation, o.Metageneration)
		}
		if in.team != "" {
			a.ProjectTeam = &storage.ObjectAccessControlProjectTeam{ProjectNumber: in.projNum, Team: in.team}
		}
		out = append(out, a)
	}
	return out
}

// fullProjection reports whether a request asks for projection=full,
// falling back to def.
func fullProjection(r *http.Request, def bool) bool {
	switch r.URL.Query().Get("projection") {
	case "full":
		return true
	case "noAcl":
		return false
	}
	return def
}

// aclKind says which ACL a request addresses.
type aclKind int

const (
	bucketACL aclKind = iota
	defaultObjectACL
	objectACL
)

// aclScope identifies one ACL: a bucket's, its default object ACL, or an
// object generation's (gen 0 = live).
type aclScope struct {
	kind   aclKind
	bucket string
	object string
	gen    int64
}

// permissions returns the permissions that guard reading and changing the
// ACL, and the resource they are checked on.
func (sc aclScope) permissions() (read, write, resource string) {
	switch sc.kind {
	case bucketACL:
		return "storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy", bucketResource(sc.bucket)
	case defaultObjectACL:
		return "storage.buckets.getIamPolicy", "storage.buckets.update", bucketResource(sc.bucket)
	}
	return "storage.objects.getIamPolicy", "storage.objects.setIamPolicy", objectResource(sc.bucket, sc.object)
}

// ublaErr is the error for ACL access under uniform bucket-level access.
func (sc aclScope) ublaErr(write bool) error {
	msgs := map[aclKind][2]string{
		bucketACL:        {"Cannot get legacy ACL for a bucket that has uniform bucket-level access.", errBucketACLUBLA},
		defaultObjectACL: {"Cannot get legacy default object ACL for a bucket that has uniform bucket-level access.", "Cannot use ACL API to set default object ACL when uniform bucket-level access is enabled."},
		objectACL:        {"Cannot get legacy ACL for an object when uniform bucket-level access is enabled.", errObjectACLUBLA},
	}[sc.kind]
	if write {
		return errInvalid("%s%s", msgs[1], ublaDoc)
	}
	return errInvalid("%s%s", msgs[0], ublaDoc)
}

const (
	errBucketACLUBLA = "Cannot use ACL API to update bucket policy when uniform bucket-level access is enabled."
	errObjectACLUBLA = "Cannot insert legacy ACL for an object when uniform bucket-level access is enabled."
)

// serveACL handles the bucketAccessControls, defaultObjectAccessControls
// and objectAccessControls collections. entity is empty for the collection.
func (s *Service) serveACL(w http.ResponseWriter, r *http.Request, sc aclScope, entity string) {
	if sc.kind == objectACL {
		gen, err := parseGeneration(r.URL.Query().Get("generation"))
		if err != nil {
			apierr.Write(w, err)
			return
		}
		sc.gen = gen
	}
	read, write, res := sc.permissions()
	m := r.Method
	switch {
	case m == http.MethodGet:
		if err := s.check(r.Context(), read, res); err != nil {
			apierr.Write(w, err)
			return
		}
		s.getACL(w, r, sc, entity)
	case entity == "" && m == http.MethodPost, entity != "" && (m == http.MethodPut || m == http.MethodPatch || m == http.MethodDelete):
		if err := s.check(r.Context(), write, res); err != nil {
			apierr.Write(w, err)
			return
		}
		s.changeACL(w, r, sc, entity)
	default:
		methodNotAllowed(w, r)
	}
}

// aclView is an ACL with what is needed to render it.
type aclView struct {
	acl []aclEntry
	bkt *storage.Bucket
	obj *storage.Object
}

func (v aclView) render(sc aclScope, base string) []any {
	var out []any
	if sc.kind == bucketACL {
		for _, a := range renderBucketACL(v.acl, v.bkt, base) {
			out = append(out, a)
		}
		return out
	}
	for _, a := range renderObjectACL(v.acl, v.bkt, v.obj, base) {
		out = append(out, a)
	}
	return out
}

func (s *Service) loadACL(sc aclScope) (aclView, error) {
	var v aclView
	err := s.env.Store.View(func(tx store.Tx) error {
		brec, err := loadBucket(tx, sc.bucket)
		if err != nil {
			return err
		}
		v.bkt = brec.Bucket
		switch sc.kind {
		case bucketACL:
			v.acl = brec.ACL
		case defaultObjectACL:
			v.acl = brec.DefaultObjectACL
		case objectACL:
			rec, _, err := loadObject(tx, sc.bucket, sc.object, sc.gen)
			if err != nil {
				return err
			}
			if rec == nil {
				return errObjectNotFound(sc.bucket, sc.object)
			}
			v.acl, v.obj = rec.ACL, rec.Object
		}
		if ublaEnabled(v.bkt) {
			return sc.ublaErr(false)
		}
		return nil
	})
	return v, err
}

func (s *Service) getACL(w http.ResponseWriter, r *http.Request, sc aclScope, entity string) {
	v, err := s.loadACL(sc)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	items := v.render(sc, baseURL(r))
	if entity == "" {
		kind := "storage#objectAccessControls"
		if sc.kind == bucketACL {
			kind = "storage#bucketAccessControls"
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "items": items})
		return
	}
	for i, e := range v.acl {
		if e.Entity == entity {
			writeJSON(w, http.StatusOK, items[i])
			return
		}
	}
	apierr.Write(w, apierr.NotFound("Not Found").WithLegacy("notFound"))
}

// changeACL inserts (POST), sets (PUT, PATCH) or removes (DELETE) one entry.
func (s *Service) changeACL(w http.ResponseWriter, r *http.Request, sc aclScope, entity string) {
	var in aclEntry
	if r.Method != http.MethodDelete {
		if err := decodeJSON(r, &in); err != nil {
			apierr.Write(w, err)
			return
		}
		if entity != "" {
			in.Entity = entity
		}
		if err := checkEntry(in, sc.kind != bucketACL); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	edit := func(acl []aclEntry) ([]aclEntry, error) {
		if r.Method != http.MethodDelete {
			return upsertEntry(acl, in), nil
		}
		for i, e := range acl {
			if e.Entity == entity {
				return append(append([]aclEntry(nil), acl[:i]...), acl[i+1:]...), nil
			}
		}
		return nil, apierr.NotFound("Not Found").WithLegacy("notFound")
	}
	v, err := s.editACL(r.Context(), sc, baseURL(r), edit)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	items := v.render(sc, baseURL(r))
	for i, e := range v.acl {
		if e.Entity == in.Entity {
			writeJSON(w, http.StatusOK, items[i])
			return
		}
	}
}

// editACL applies edit to an ACL. A bucket ACL change bumps the bucket's
// metageneration, an object ACL change the object's.
func (s *Service) editACL(ctx context.Context, sc aclScope, base string, edit func([]aclEntry) ([]aclEntry, error)) (aclView, error) {
	var v aclView
	if sc.kind == objectACL {
		rec, bkt, err := s.updateObjectMeta(ctx, sc.bucket, sc.object, sc.gen, conds{}, base, false, func(rec *objectRec, bkt *storage.Bucket) (*storage.Object, error) {
			if ublaEnabled(bkt) {
				return nil, sc.ublaErr(true)
			}
			acl, err := edit(rec.ACL)
			if err != nil {
				return nil, err
			}
			rec.ACL = acl
			o := *rec.Object
			return &o, nil
		})
		if err != nil {
			return v, err
		}
		return aclView{acl: rec.ACL, bkt: bkt, obj: rec.Object}, nil
	}
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, err := loadBucket(tx, sc.bucket)
		if err != nil {
			return err
		}
		if ublaEnabled(rec.Bucket) {
			return sc.ublaErr(true)
		}
		cur := &rec.ACL
		if sc.kind == defaultObjectACL {
			cur = &rec.DefaultObjectACL
		}
		acl, err := edit(*cur)
		if err != nil {
			return err
		}
		*cur = acl
		rec.Bucket.Metageneration++
		rec.Bucket.Updated = ts(s.now())
		v = aclView{acl: acl, bkt: rec.Bucket}
		return store.PutJSON(tx, nsBuckets, sc.bucket, rec)
	})
	return v, err
}

// bucketACLChange is the ACL part of a bucket insert, patch or update: ACL
// lists from the body (nil = unchanged) and predefined ACL parameters.
type bucketACLChange struct {
	acl, defaultObjectACL            []aclEntry
	setACL, setDefaultObjectACL      bool
	predefined, predefinedDefaultObj string
}

// parseBucketACLChange reads the ACL fields of a bucket request body and
// the predefinedAcl/predefinedDefaultObjectAcl parameters.
func parseBucketACLChange(body map[string]json.RawMessage, q url.Values) (*bucketACLChange, error) {
	c := &bucketACLChange{predefined: q.Get("predefinedAcl"), predefinedDefaultObj: q.Get("predefinedDefaultObjectAcl")}
	if raw, ok := body["acl"]; ok {
		var items []*storage.BucketAccessControl
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, errInvalid("Invalid value for field 'acl'.")
		}
		acl, err := entriesOf(items, false, bucketPair)
		if err != nil {
			return nil, err
		}
		c.acl, c.setACL = acl, true
	}
	if raw, ok := body["defaultObjectAcl"]; ok {
		var items []*storage.ObjectAccessControl
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, errInvalid("Invalid value for field 'defaultObjectAcl'.")
		}
		acl, err := entriesOf(items, true, objectPair)
		if err != nil {
			return nil, err
		}
		c.defaultObjectACL, c.setDefaultObjectACL = acl, true
	}
	return c, nil
}

// changes reports whether the request changes an ACL.
func (c *bucketACLChange) changes() bool {
	return c.setACL || c.setDefaultObjectACL || c.predefined != "" || c.predefinedDefaultObj != ""
}

// apply sets rec's ACLs. b is the bucket as it will be stored.
func (c *bucketACLChange) apply(rec *bucketRec, b *storage.Bucket) error {
	if !c.changes() {
		return nil
	}
	if ublaEnabled(b) {
		return errInvalid("%s%s", errBucketACLUBLA, ublaDoc)
	}
	switch {
	case c.predefined != "":
		acl, err := predefinedBucketACL(c.predefined, b.ProjectNumber)
		if err != nil {
			return err
		}
		rec.ACL = acl
	case c.setACL:
		rec.ACL = c.acl
	}
	switch {
	case c.predefinedDefaultObj != "":
		acl, err := predefinedObjectACL(c.predefinedDefaultObj, b.ProjectNumber, "")
		if err != nil {
			return err
		}
		rec.DefaultObjectACL = acl
	case c.setDefaultObjectACL:
		rec.DefaultObjectACL = c.defaultObjectACL
	}
	return nil
}

// parseObjectACLChange reads the acl field of an object patch or update and
// its predefinedAcl parameter. It returns nil when neither is set, else a
// function computing the new ACL from the project number and owner.
func parseObjectACLChange(body map[string]json.RawMessage, predefined string) (func(projNum uint64, owner string) ([]aclEntry, error), error) {
	if predefined != "" {
		return func(projNum uint64, owner string) ([]aclEntry, error) {
			return predefinedObjectACL(predefined, projNum, owner)
		}, nil
	}
	raw, ok := body["acl"]
	if !ok {
		return nil, nil
	}
	var items []*storage.ObjectAccessControl
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errInvalid("Invalid value for field 'acl'.")
	}
	acl, err := entriesOf(items, true, objectPair)
	if err != nil {
		return nil, err
	}
	return func(uint64, string) ([]aclEntry, error) { return acl, nil }, nil
}

// fillACLDefaults gives a bucket stored before ACLs were kept the ACLs GCS
// gives a new bucket.
func fillACLDefaults(rec *bucketRec) {
	if rec.Owner != "" {
		return
	}
	n := rec.Bucket.ProjectNumber
	rec.Owner = projectEntity("owners", n)
	rec.ACL = projectPrivate(n)
	rec.DefaultObjectACL = projectPrivate(n)
}

// newObjectACL returns the ACL of a new object generation: the request's
// predefinedAcl or acl, otherwise the bucket's default object ACL, with the
// owner as OWNER.
func newObjectACL(wr *writeReq, brec *bucketRec, owner string) ([]aclEntry, error) {
	if wr.predefinedACL == "" && len(wr.meta.Acl) == 0 {
		return withOwner(append([]aclEntry(nil), brec.DefaultObjectACL...), owner), nil
	}
	if ublaEnabled(brec.Bucket) {
		return nil, errInvalid("%s%s", errObjectACLUBLA, ublaDoc)
	}
	if wr.predefinedACL != "" {
		return predefinedObjectACL(wr.predefinedACL, brec.Bucket.ProjectNumber, owner)
	}
	acl, err := entriesOf(wr.meta.Acl, true, objectPair)
	if err != nil {
		return nil, err
	}
	return withOwner(acl, owner), nil
}
