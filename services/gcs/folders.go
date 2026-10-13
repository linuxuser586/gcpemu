package gcs

import (
	"net/http"
	"net/url"
	"strings"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Managed folders: create, get, list, delete and IAM policies. Objects
// follow the policies of the managed folders containing them as well as
// their bucket's: the iam service puts those folders in an object's policy
// chain.

// folderRec is the persisted form of a managed folder.
type folderRec struct {
	Folder *storage.ManagedFolder `json:"folder"`
	// PolicySet records whether setIamPolicy was ever called.
	PolicySet bool `json:"policySet,omitempty"`
}

// folderResource is the IAM full resource name of a managed folder. name
// ends in "/".
func folderResource(bucket, name string) string {
	return bucketResource(bucket) + "/managedFolders/" + name
}

func errFolderNotFound() *apierr.Error {
	return apierr.NotFound("The specified managed folder does not exist.")
}

func loadFolder(tx store.Tx, bucket, name string) (*folderRec, error) {
	var rec folderRec
	if err := store.GetJSON(tx, nsFolders, objKey(bucket, name), &rec); err != nil {
		if err == store.ErrNotFound {
			return nil, errFolderNotFound()
		}
		return nil, err
	}
	return &rec, nil
}

func folderTarget(bucket string, rec *folderRec) *policyTarget {
	name := rec.Folder.Name
	return &policyTarget{
		resource:   folderResource(bucket, name),
		resourceID: "projects/_/buckets/" + bucket + "/managedFolders/" + name,
		localKey:   objKey(bucket, name),
		set:        rec.PolicySet,
		def:        &storage.Policy{Etag: bucketEtag(1), Version: 1},
		markSet: func(tx store.Tx) error {
			f, err := loadFolder(tx, bucket, name)
			if err != nil || f.PolicySet {
				return err
			}
			f.PolicySet = true
			return store.PutJSON(tx, nsFolders, objKey(bucket, name), f)
		},
	}
}

func renderFolder(rec *folderRec, base string) *storage.ManagedFolder {
	f := *rec.Folder
	f.SelfLink = base + "/storage/v1/b/" + f.Bucket + "/managedFolders/" + url.PathEscape(f.Name)
	return &f
}

// folderName normalizes a managed folder name to end in "/" and validates it.
func folderName(name string) (string, error) {
	if name == "" {
		return "", errRequired("name")
	}
	if !strings.HasSuffix(name, "/") {
		name += "/"
	}
	if strings.HasPrefix(name, "/") || strings.Contains(name, "//") || !validObjectName(name) {
		return "", errInvalid("The specified managed folder name is not valid.")
	}
	return name, nil
}

// serveManagedFolders routes ".../b/B/managedFolders[/F[/iam[/testPermissions]]]".
// esc holds the escaped segments after "managedFolders"; F is one escaped
// segment, or several when the client left its slashes unescaped.
func (s *Service) serveManagedFolders(w http.ResponseWriter, r *http.Request, bucket string, esc []string) {
	m := r.Method
	if len(esc) == 0 || (len(esc) == 1 && esc[0] == "") {
		switch m {
		case http.MethodGet:
			s.listFolders(w, r, bucket)
		case http.MethodPost:
			s.insertFolder(w, r, bucket)
		default:
			methodNotAllowed(w, r)
		}
		return
	}
	var sub string
	switch n := len(esc); {
	case n >= 3 && esc[n-2] == "iam" && esc[n-1] == "testPermissions":
		sub, esc = "testPermissions", esc[:n-2]
	case n >= 2 && esc[n-1] == "iam":
		sub, esc = "iam", esc[:n-1]
	}
	raw, ok := joinUnescaped(esc)
	if !ok {
		apierr.Write(w, errInvalid("Invalid path."))
		return
	}
	name, err := folderName(raw)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	res := folderResource(bucket, name)
	switch {
	case sub == "" && m == http.MethodGet:
		if err := s.check(r.Context(), "storage.managedFolders.get", res); err != nil {
			apierr.Write(w, err)
			return
		}
		rec, err := s.getFolderRec(bucket, name)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		writeJSON(w, http.StatusOK, renderFolder(rec, baseURL(r)))
	case sub == "" && m == http.MethodDelete:
		s.deleteFolder(w, r, bucket, name)
	case sub == "iam" && m == http.MethodGet:
		if err := s.check(r.Context(), "storage.managedFolders.getIamPolicy", res); err != nil {
			apierr.Write(w, err)
			return
		}
		rec, err := s.getFolderRec(bucket, name)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		s.writePolicy(w, r, folderTarget(bucket, rec))
	case sub == "iam" && m == http.MethodPut:
		s.setFolderIAM(w, r, bucket, name)
	case sub == "testPermissions" && m == http.MethodGet:
		if _, err := s.getFolderRec(bucket, name); err != nil {
			apierr.Write(w, err)
			return
		}
		s.testIAM(w, r, res)
	default:
		methodNotAllowed(w, r)
	}
}

func (s *Service) getFolderRec(bucket, name string) (*folderRec, error) {
	var rec *folderRec
	err := s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		var err error
		rec, err = loadFolder(tx, bucket, name)
		return err
	})
	return rec, err
}

func (s *Service) insertFolder(w http.ResponseWriter, r *http.Request, bucket string) {
	var in storage.ManagedFolder
	if err := decodeJSON(r, &in); err != nil {
		apierr.Write(w, err)
		return
	}
	name, err := folderName(in.Name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "storage.managedFolders.create", folderResource(bucket, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	now := ts(s.now())
	rec := &folderRec{Folder: &storage.ManagedFolder{
		Kind:           "storage#managedFolder",
		Id:             bucket + "/" + name,
		Name:           name,
		Bucket:         bucket,
		CreateTime:     now,
		UpdateTime:     now,
		Metageneration: 1,
	}}
	err = s.env.Store.Update(func(tx store.Tx) error {
		brec, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		if !ublaEnabled(brec.Bucket) {
			return errInvalid("Managed folders can only be created in buckets that have uniform bucket-level access enabled.")
		}
		if store.Exists(tx, nsFolders, objKey(bucket, name)) {
			return apierr.AlreadyExists("The managed folder you tried to create already exists.")
		}
		return store.PutJSON(tx, nsFolders, objKey(bucket, name), rec)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderFolder(rec, baseURL(r)))
}

func (s *Service) listFolders(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.check(r.Context(), "storage.managedFolders.list", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	q := r.URL.Query()
	max := pageSize(q.Get("pageSize"), 1000)
	marker, _ := decodeToken(q.Get("pageToken"))
	out := &storage.ManagedFolders{Kind: "storage#managedFolders"}
	base := baseURL(r)
	err := s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		var ierr error
		tx.Scan(nsFolders, objPrefix(bucket, q.Get("prefix")), func(key string, val []byte) bool {
			var rec folderRec
			if ierr = jsonUnmarshal(val, &rec); ierr != nil {
				return false
			}
			if marker != "" && rec.Folder.Name <= marker {
				return true
			}
			if len(out.Items) == max {
				out.NextPageToken = encodeToken(out.Items[len(out.Items)-1].Name)
				return false
			}
			out.Items = append(out.Items, renderFolder(&rec, base))
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

// deleteFolder deletes a managed folder and its policy. Unless
// allowNonEmpty is set, the folder must contain no objects or managed
// folders.
func (s *Service) deleteFolder(w http.ResponseWriter, r *http.Request, bucket, name string) {
	ctx := r.Context()
	if err := s.check(ctx, "storage.managedFolders.delete", folderResource(bucket, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	q := r.URL.Query()
	c, err := parseConds(q, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	allowNonEmpty := q.Get("allowNonEmpty") == "true"
	err = s.env.Store.Update(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		rec, err := loadFolder(tx, bucket, name)
		if err != nil {
			return err
		}
		mg := rec.Folder.Metageneration
		if (c.MetagenMatch != nil && *c.MetagenMatch != mg) || (c.MetagenNotMatch != nil && *c.MetagenNotMatch == mg) {
			return errPrecondition()
		}
		if !allowNonEmpty {
			nested := false
			tx.Scan(nsFolders, objPrefix(bucket, name), func(k string, _ []byte) bool {
				nested = k != objKey(bucket, name)
				return !nested
			})
			if nested || store.HasPrefix(tx, nsObjects, objPrefix(bucket, name)) {
				return errConflict("The managed folder you tried to delete is not empty.")
			}
		}
		_ = tx.Delete(nsIAM, objKey(bucket, name))
		return tx.Delete(nsFolders, objKey(bucket, name))
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if ps := s.policyStore(); ps != nil {
		_ = ps.DeletePolicy(ctx, folderResource(bucket, name))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) setFolderIAM(w http.ResponseWriter, r *http.Request, bucket, name string) {
	ctx := r.Context()
	if err := s.check(ctx, "storage.managedFolders.setIamPolicy", folderResource(bucket, name)); err != nil {
		apierr.Write(w, err)
		return
	}
	var p storage.Policy
	if err := decodeJSON(r, &p); err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.getFolderRec(bucket, name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	t := folderTarget(bucket, rec)
	out, err := s.putPolicy(ctx, t, &p)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderPolicy(out, t.resourceID))
}
