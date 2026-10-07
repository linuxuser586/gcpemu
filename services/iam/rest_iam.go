package iam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// serveIAM serves iam.googleapis.com v1 (FR-IAM-001..003).
func (s *Service) serveIAM(w http.ResponseWriter, r *http.Request) {
	rt := parseRoute(r.URL.Path)
	if len(rt.segs) == 0 || rt.segs[0] != "v1" {
		notFoundRoute(w, r)
		return
	}
	err := s.routeIAM(w, r, rt)
	if err != nil {
		apierr.Write(w, err)
	}
}

func (s *Service) routeIAM(w http.ResponseWriter, r *http.Request, rt route) error {
	ctx := r.Context()
	m := r.Method
	if _, ok := rt.match("", "v1", "roles"); ok && m == http.MethodGet {
		return s.listPredefinedRoles(w, r)
	}
	if c, ok := rt.match("", "v1", "roles", "*"); ok && m == http.MethodGet {
		return s.getPredefinedRole(w, "roles/"+c[0])
	}
	if _, ok := rt.match("queryGrantableRoles", "v1", "roles"); ok && m == http.MethodPost {
		return s.queryGrantableRoles(w, r)
	}
	if _, ok := rt.match("queryTestablePermissions", "v1", "permissions"); ok && m == http.MethodPost {
		return s.queryTestablePermissions(w, r)
	}
	if c, ok := rt.match("", "v1", "projects", "*", "roles"); ok {
		switch m {
		case http.MethodGet:
			return s.listCustomRoles(w, r, c[0])
		case http.MethodPost:
			return s.createCustomRole(w, r, c[0])
		}
	}
	if c, ok := rt.match("", "v1", "projects", "*", "roles", "*"); ok {
		switch m {
		case http.MethodGet:
			return s.getCustomRole(w, r, c[0], c[1])
		case http.MethodPatch:
			return s.patchCustomRole(w, r, c[0], c[1])
		case http.MethodDelete:
			return s.deleteCustomRole(w, r, c[0], c[1])
		}
	}
	if c, ok := rt.match("undelete", "v1", "projects", "*", "roles", "*"); ok && m == http.MethodPost {
		return s.undeleteCustomRole(w, r, c[0], c[1])
	}
	if c, ok := rt.match("", "v1", "projects", "*", "serviceAccounts"); ok {
		switch m {
		case http.MethodGet:
			return s.listAccounts(w, r, c[0])
		case http.MethodPost:
			return s.createAccount(w, r, c[0])
		}
	}
	if c, ok := rt.match("", "v1", "projects", "*", "serviceAccounts", "*"); ok {
		switch m {
		case http.MethodGet:
			sa, err := s.accountFor(ctx, c[0], c[1], "iam.serviceAccounts.get")
			if err != nil {
				return err
			}
			writeJSON(w, sa)
			return nil
		case http.MethodPatch:
			return s.patchAccount(w, r, c[0], c[1])
		case http.MethodPut:
			return s.updateAccount(w, r, c[0], c[1])
		case http.MethodDelete:
			return s.deleteAccount(w, r, c[0], c[1])
		}
	}
	if c, ok := rt.match(rt.verb, "v1", "projects", "*", "serviceAccounts", "*"); ok && rt.verb != "" && m == http.MethodPost {
		return s.accountVerb(w, r, c[0], c[1], rt.verb)
	}
	if c, ok := rt.match("", "v1", "projects", "*", "serviceAccounts", "*", "keys"); ok {
		switch m {
		case http.MethodGet:
			return s.listKeysREST(w, r, c[0], c[1])
		case http.MethodPost:
			return s.createKeyREST(w, r, c[0], c[1])
		}
	}
	if c, ok := rt.match("upload", "v1", "projects", "*", "serviceAccounts", "*", "keys"); ok && m == http.MethodPost {
		return s.uploadKeyREST(w, r, c[0], c[1])
	}
	if c, ok := rt.match("", "v1", "projects", "*", "serviceAccounts", "*", "keys", "*"); ok {
		switch m {
		case http.MethodGet:
			return s.getKeyREST(w, r, c[0], c[1], c[2])
		case http.MethodDelete:
			return s.deleteKeyREST(w, r, c[0], c[1], c[2])
		}
	}
	if c, ok := rt.match(rt.verb, "v1", "projects", "*", "serviceAccounts", "*", "keys", "*"); ok && (rt.verb == "disable" || rt.verb == "enable") && m == http.MethodPost {
		return s.setKeyDisabled(w, r, c[0], c[1], c[2], rt.verb == "disable")
	}
	if len(rt.segs) >= 5 && rt.segs[1] == "projects" && rt.segs[3] == "locations" {
		return s.routeWorkloadIdentity(w, r, rt)
	}
	notFoundRoute(w, r)
	return nil
}

// --- service accounts (FR-IAM-001) ---

func (s *Service) check(ctx context.Context, perm, res string) error {
	return s.env.Auth.Check(ctx, perm, res)
}

// accountFor resolves projects/{p}/serviceAccounts/{id} after checking perm
// on it; a missing account is reported after the permission check, as GCP
// does.
func (s *Service) accountFor(ctx context.Context, projectSeg, id, perm string) (*iamv1.ServiceAccount, error) {
	var sa *iamv1.ServiceAccount
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { sa, ok = s.getAccount(tx, id); return nil })
	res := ""
	if ok {
		res = saResource(sa)
	} else {
		p := projectOfEmail(id)
		if p == "" {
			p = projectSeg
		}
		res = "//iam.googleapis.com/projects/" + p + "/serviceAccounts/" + id
	}
	if err := s.check(ctx, perm, res); err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFoundSA()
	}
	return sa, nil
}

func (s *Service) createAccount(w http.ResponseWriter, r *http.Request, projectSeg string) error {
	ctx := r.Context()
	var req iamv1.CreateServiceAccountRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	pid := projectSeg
	_ = s.env.Store.View(func(tx store.Tx) error { pid = s.projectID(tx, projectSeg); return nil })
	if err := s.env.EnsureProject(pid); err != nil {
		return err
	}
	if err := s.check(ctx, "iam.serviceAccounts.create", projectResource(pid)); err != nil {
		return err
	}
	sa, err := s.newAccount(pid, req.AccountId, req.ServiceAccount)
	if err != nil {
		return err
	}
	writeJSON(w, sa)
	return nil
}

// newAccount creates and stores a service account.
func (s *Service) newAccount(pid, accountID string, in *iamv1.ServiceAccount) (*iamv1.ServiceAccount, error) {
	if len(accountID) < 6 || len(accountID) > 30 || !accountIDRE.MatchString(accountID) {
		return nil, apierr.InvalidArgument("Invalid account ID: %q. The account ID must be between 6 and 30 characters, start with a lowercase letter, and contain only lowercase letters, digits and dashes.", accountID).
			WithReason(iamDomain, "INVALID_ACCOUNT_ID")
	}
	if in == nil {
		in = &iamv1.ServiceAccount{}
	}
	if len(in.DisplayName) > 100 {
		return nil, apierr.InvalidArgument("Display name must be at most 100 characters.")
	}
	if len(in.Description) > 256 {
		return nil, apierr.InvalidArgument("Description must be at most 256 characters.")
	}
	email := saEmail(pid, accountID)
	uid := "10" + s.env.IDs.Numeric()
	sa := &iamv1.ServiceAccount{
		Name:           "projects/" + pid + "/serviceAccounts/" + email,
		ProjectId:      pid,
		UniqueId:       uid,
		Email:          email,
		DisplayName:    in.DisplayName,
		Description:    in.Description,
		Etag:           saEtag,
		Oauth2ClientId: uid,
	}
	err := s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsAccounts, email) {
			return apierr.AlreadyExists("Service account %s already exists within project projects/%s.", accountID, pid).
				WithReason(iamDomain, "SERVICE_ACCOUNT_ALREADY_EXISTS")
		}
		return store.PutJSON(tx, nsAccounts, email, sa)
	})
	return sa, err
}

func (s *Service) listAccounts(w http.ResponseWriter, r *http.Request, projectSeg string) error {
	ctx := r.Context()
	pid := projectSeg
	_ = s.env.Store.View(func(tx store.Tx) error { pid = s.projectID(tx, projectSeg); return nil })
	if err := s.env.EnsureProject(pid); err != nil {
		return err
	}
	if err := s.check(ctx, "iam.serviceAccounts.list", projectResource(pid)); err != nil {
		return err
	}
	size, off, err := pageParams(r, 100, 100)
	if err != nil {
		return err
	}
	var all []*iamv1.ServiceAccount
	_ = s.env.Store.View(func(tx store.Tx) error {
		list, err := store.ListJSON[*iamv1.ServiceAccount](tx, nsAccounts, "")
		for _, sa := range list {
			if sa.ProjectId == pid {
				all = append(all, sa)
			}
		}
		return err
	})
	items, next := page(all, size, off)
	writeJSON(w, &iamv1.ListServiceAccountsResponse{Accounts: items, NextPageToken: next})
	return nil
}

// mutateAccount applies fn to a stored account under perm.
func (s *Service) mutateAccount(ctx context.Context, projectSeg, id, perm string, fn func(sa *iamv1.ServiceAccount) error) (*iamv1.ServiceAccount, error) {
	sa, err := s.accountFor(ctx, projectSeg, id, perm)
	if err != nil {
		return nil, err
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		var cur iamv1.ServiceAccount
		if store.GetJSON(tx, nsAccounts, sa.Email, &cur) != nil {
			return notFoundSA()
		}
		if err := fn(&cur); err != nil {
			return err
		}
		sa = &cur
		return store.PutJSON(tx, nsAccounts, sa.Email, sa)
	})
	return sa, err
}

func (s *Service) patchAccount(w http.ResponseWriter, r *http.Request, projectSeg, id string) error {
	var req iamv1.PatchServiceAccountRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.ServiceAccount == nil {
		return apierr.InvalidArgument("serviceAccount is required.")
	}
	mask := req.UpdateMask
	if mask == "" {
		mask = r.URL.Query().Get("updateMask")
	}
	if mask == "" {
		return apierr.InvalidArgument("Field mask must not be empty.").WithReason(iamDomain, "INVALID_ARGUMENT")
	}
	sa, err := s.mutateAccount(r.Context(), projectSeg, id, "iam.serviceAccounts.update", func(sa *iamv1.ServiceAccount) error {
		for _, f := range strings.Split(mask, ",") {
			switch strings.TrimSpace(f) {
			case "displayName", "display_name":
				if len(req.ServiceAccount.DisplayName) > 100 {
					return apierr.InvalidArgument("Display name must be at most 100 characters.")
				}
				sa.DisplayName = req.ServiceAccount.DisplayName
			case "description":
				if len(req.ServiceAccount.Description) > 256 {
					return apierr.InvalidArgument("Description must be at most 256 characters.")
				}
				sa.Description = req.ServiceAccount.Description
			default:
				return apierr.InvalidArgument("Invalid update mask field: %s", f)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, sa)
	return nil
}

func (s *Service) updateAccount(w http.ResponseWriter, r *http.Request, projectSeg, id string) error {
	var in iamv1.ServiceAccount
	if err := readJSON(r, &in); err != nil {
		return err
	}
	sa, err := s.mutateAccount(r.Context(), projectSeg, id, "iam.serviceAccounts.update", func(sa *iamv1.ServiceAccount) error {
		sa.DisplayName, sa.Description = in.DisplayName, in.Description
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, sa)
	return nil
}

func (s *Service) deleteAccount(w http.ResponseWriter, r *http.Request, projectSeg, id string) error {
	sa, err := s.accountFor(r.Context(), projectSeg, id, "iam.serviceAccounts.delete")
	if err != nil {
		return err
	}
	if err := s.removeAccount(sa); err != nil {
		return err
	}
	writeJSON(w, struct{}{})
	return nil
}

// removeAccount soft-deletes an account (kept for undelete), its keys and
// its policy.
func (s *Service) removeAccount(sa *iamv1.ServiceAccount) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		if !store.Exists(tx, nsAccounts, sa.Email) {
			return notFoundSA()
		}
		for _, k := range listKeys(tx, sa.Email) {
			_ = tx.Delete(nsKeys, keyKey(k.Email, k.KeyID))
		}
		_ = tx.Delete(nsPolicies, saResource(sa))
		if err := store.PutJSON(tx, nsDeleted, sa.UniqueId, deletedAccount{Account: sa, DeleteTime: s.env.Clock.Now()}); err != nil {
			return err
		}
		return tx.Delete(nsAccounts, sa.Email)
	})
}

// accountVerb serves the custom methods on a service account.
func (s *Service) accountVerb(w http.ResponseWriter, r *http.Request, projectSeg, id, verb string) error {
	ctx := r.Context()
	switch verb {
	case "disable", "enable":
		_, err := s.mutateAccount(ctx, projectSeg, id, "iam.serviceAccounts."+verb, func(sa *iamv1.ServiceAccount) error {
			sa.Disabled = verb == "disable"
			return nil
		})
		if err != nil {
			return err
		}
		writeJSON(w, struct{}{})
		return nil
	case "undelete":
		return s.undeleteAccount(w, r, id)
	case "getIamPolicy":
		sa, err := s.accountFor(ctx, projectSeg, id, "iam.serviceAccounts.getIamPolicy")
		if err != nil {
			return err
		}
		var p *iamv1.Policy
		_ = s.env.Store.View(func(tx store.Tx) error { p = loadPolicy(tx, saResource(sa)); return nil })
		writeJSON(w, p)
		return nil
	case "setIamPolicy":
		var req iamv1.SetIamPolicyRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		sa, err := s.accountFor(ctx, projectSeg, id, "iam.serviceAccounts.setIamPolicy")
		if err != nil {
			return err
		}
		var out *iamv1.Policy
		err = s.env.Store.Update(func(tx store.Tx) error {
			out, err = s.storePolicy(tx, saResource(sa), req.Policy, req.UpdateMask)
			return err
		})
		if err != nil {
			return err
		}
		writeJSON(w, out)
		return nil
	case "testIamPermissions":
		var req iamv1.TestIamPermissionsRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		var sa *iamv1.ServiceAccount
		var ok bool
		_ = s.env.Store.View(func(tx store.Tx) error { sa, ok = s.getAccount(tx, id); return nil })
		if !ok {
			return notFoundSA()
		}
		writeJSON(w, &iamv1.TestIamPermissionsResponse{Permissions: s.TestPermissions(ctx, saResource(sa), req.Permissions)})
		return nil
	case "signBlob":
		var req iamv1.SignBlobRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		sa, err := s.accountFor(ctx, projectSeg, id, "iam.serviceAccounts.signBlob")
		if err != nil {
			return err
		}
		data, err := base64.StdEncoding.DecodeString(req.BytesToSign)
		if err != nil {
			return apierr.InvalidArgument("bytesToSign is not valid base64.")
		}
		kid, sig, err := s.signWithSystemKey(sa, data)
		if err != nil {
			return err
		}
		writeJSON(w, &iamv1.SignBlobResponse{KeyId: kid, Signature: base64.StdEncoding.EncodeToString(sig)})
		return nil
	case "signJwt":
		var req iamv1.SignJwtRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		sa, err := s.accountFor(ctx, projectSeg, id, "iam.serviceAccounts.signJwt")
		if err != nil {
			return err
		}
		kid, jwt, err := s.signJWTWithSystemKey(sa, req.Payload)
		if err != nil {
			return apierr.InvalidArgument("%v", err)
		}
		writeJSON(w, &iamv1.SignJwtResponse{KeyId: kid, SignedJwt: jwt})
		return nil
	}
	return apierr.NotFound("Method %s not found.", verb)
}

func (s *Service) undeleteAccount(w http.ResponseWriter, r *http.Request, id string) error {
	var del deletedAccount
	var found bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		found = store.GetJSON(tx, nsDeleted, id, &del) == nil
		if !found && strings.Contains(id, "@") {
			tx.Scan(nsDeleted, "", func(_ string, b []byte) bool {
				var d deletedAccount
				if json.Unmarshal(b, &d) == nil && strings.EqualFold(d.Account.Email, id) {
					del, found = d, true
				}
				return true
			})
		}
		return nil
	})
	res := "//iam.googleapis.com/projects/-/serviceAccounts/" + id
	if found {
		res = saResource(del.Account)
	}
	if err := s.check(r.Context(), "iam.serviceAccounts.undelete", res); err != nil {
		return err
	}
	if !found {
		return apierr.NotFound("Account deleted: %s", id).WithReason(iamDomain, "SERVICE_ACCOUNT_NOT_FOUND")
	}
	sa := del.Account
	err := s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsAccounts, sa.Email) {
			return apierr.FailedPrecondition("Account with the same email %s already exists.", sa.Email)
		}
		if err := tx.Delete(nsDeleted, sa.UniqueId); err != nil {
			return err
		}
		return store.PutJSON(tx, nsAccounts, sa.Email, sa)
	})
	if err != nil {
		return err
	}
	writeJSON(w, &iamv1.UndeleteServiceAccountResponse{RestoredAccount: sa})
	return nil
}

// --- keys (FR-IAM-001) ---

func (s *Service) listKeysREST(w http.ResponseWriter, r *http.Request, projectSeg, id string) error {
	sa, err := s.accountFor(r.Context(), projectSeg, id, "iam.serviceAccountKeys.list")
	if err != nil {
		return err
	}
	types := r.URL.Query()["keyTypes"]
	var out []*iamv1.ServiceAccountKey
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, k := range listKeys(tx, sa.Email, types...) {
			out = append(out, k.toAPI(""))
		}
		return nil
	})
	writeJSON(w, &iamv1.ListServiceAccountKeysResponse{Keys: out})
	return nil
}

func (s *Service) createKeyREST(w http.ResponseWriter, r *http.Request, projectSeg, id string) error {
	var req iamv1.CreateServiceAccountKeyRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	sa, err := s.accountFor(r.Context(), projectSeg, id, "iam.serviceAccountKeys.create")
	if err != nil {
		return err
	}
	switch req.PrivateKeyType {
	case "", "TYPE_GOOGLE_CREDENTIALS_FILE":
		req.PrivateKeyType = "TYPE_GOOGLE_CREDENTIALS_FILE"
	case "TYPE_PKCS12_FILE":
		return apierr.Unimplemented("PKCS12 keys are not supported by the emulator; use TYPE_GOOGLE_CREDENTIALS_FILE.")
	default:
		return apierr.InvalidArgument("Invalid private key type %s.", req.PrivateKeyType)
	}
	switch req.KeyAlgorithm {
	case "", "KEY_ALG_UNSPECIFIED", keyAlgRSA2048:
	default:
		return apierr.InvalidArgument("Unsupported key algorithm %s; only KEY_ALG_RSA_2048 is supported.", req.KeyAlgorithm)
	}
	rec, kf, err := s.createKey(sa)
	if err != nil {
		return err
	}
	out := rec.toAPI("")
	out.PrivateKeyType = req.PrivateKeyType
	out.PrivateKeyData = base64.StdEncoding.EncodeToString(kf)
	writeJSON(w, out)
	return nil
}

func (s *Service) uploadKeyREST(w http.ResponseWriter, r *http.Request, projectSeg, id string) error {
	var req iamv1.UploadServiceAccountKeyRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	sa, err := s.accountFor(r.Context(), projectSeg, id, "iam.serviceAccountKeys.create")
	if err != nil {
		return err
	}
	pemData, err := base64.StdEncoding.DecodeString(req.PublicKeyData)
	if err != nil {
		return apierr.InvalidArgument("publicKeyData is not valid base64.")
	}
	rec, err := s.uploadKey(sa, pemData)
	if err != nil {
		return err
	}
	writeJSON(w, rec.toAPI(""))
	return nil
}

// keyFor resolves a key of an account after checking perm.
func (s *Service) keyFor(ctx context.Context, projectSeg, id, keyID, perm string) (*keyRecord, error) {
	sa, err := s.accountFor(ctx, projectSeg, id, perm)
	if err != nil {
		return nil, err
	}
	var k keyRecord
	var found bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		found = store.GetJSON(tx, nsKeys, keyKey(sa.Email, keyID), &k) == nil
		return nil
	})
	if !found {
		return nil, apierr.NotFound("Service account key %s does not exist.", keyID).WithReason(iamDomain, "KEY_NOT_FOUND")
	}
	return &k, nil
}

func (s *Service) getKeyREST(w http.ResponseWriter, r *http.Request, projectSeg, id, keyID string) error {
	k, err := s.keyFor(r.Context(), projectSeg, id, keyID, "iam.serviceAccountKeys.get")
	if err != nil {
		return err
	}
	writeJSON(w, k.toAPI(r.URL.Query().Get("publicKeyType")))
	return nil
}

func (s *Service) deleteKeyREST(w http.ResponseWriter, r *http.Request, projectSeg, id, keyID string) error {
	k, err := s.keyFor(r.Context(), projectSeg, id, keyID, "iam.serviceAccountKeys.delete")
	if err != nil {
		return err
	}
	if k.Type == keySystemManaged {
		return apierr.FailedPrecondition("System-managed keys cannot be deleted.")
	}
	if err := s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsKeys, keyKey(k.Email, k.KeyID)) }); err != nil {
		return err
	}
	writeJSON(w, struct{}{})
	return nil
}

func (s *Service) setKeyDisabled(w http.ResponseWriter, r *http.Request, projectSeg, id, keyID string, disabled bool) error {
	perm := "iam.serviceAccountKeys.enable"
	if disabled {
		perm = "iam.serviceAccountKeys.disable"
	}
	k, err := s.keyFor(r.Context(), projectSeg, id, keyID, perm)
	if err != nil {
		return err
	}
	k.Disabled = disabled
	if err := s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsKeys, keyKey(k.Email, k.KeyID), k) }); err != nil {
		return err
	}
	writeJSON(w, struct{}{})
	return nil
}

// --- roles (FR-IAM-002) ---

const predefinedEtag = "AA=="

func (s *Service) predefinedToAPI(r *predefinedRole, full bool) *iamv1.Role {
	out := &iamv1.Role{Name: r.Name, Title: r.Title, Description: r.Description, Stage: r.Stage, Etag: predefinedEtag}
	if full {
		out.IncludedPermissions = r.Permissions
	}
	return out
}

func (s *Service) listPredefinedRoles(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	if parent := q.Get("parent"); strings.HasPrefix(parent, "projects/") {
		return s.listCustomRoles(w, r, strings.TrimPrefix(parent, "projects/"))
	}
	size, off, err := pageParams(r, 300, 1000)
	if err != nil {
		return err
	}
	full := q.Get("view") == "FULL"
	var all []*iamv1.Role
	for _, n := range s.cat.names {
		all = append(all, s.predefinedToAPI(s.cat.roles[n], full))
	}
	items, next := page(all, size, off)
	writeJSON(w, &iamv1.ListRolesResponse{Roles: items, NextPageToken: next})
	return nil
}

func (s *Service) getPredefinedRole(w http.ResponseWriter, name string) error {
	r, ok := s.cat.roles[name]
	if !ok {
		return apierr.NotFound("The role named %s was not found.", name).WithReason(iamDomain, "ROLE_NOT_FOUND")
	}
	writeJSON(w, s.predefinedToAPI(r, true))
	return nil
}

func (s *Service) queryGrantableRoles(w http.ResponseWriter, r *http.Request) error {
	var req iamv1.QueryGrantableRolesRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var out []*iamv1.Role
	for _, n := range s.cat.names {
		out = append(out, s.predefinedToAPI(s.cat.roles[n], req.View == "FULL"))
	}
	writeJSON(w, &iamv1.QueryGrantableRolesResponse{Roles: out})
	return nil
}

func (s *Service) queryTestablePermissions(w http.ResponseWriter, r *http.Request) error {
	var out []*iamv1.Permission
	for _, p := range s.cat.universe {
		out = append(out, &iamv1.Permission{Name: p, Stage: "GA", CustomRolesSupportLevel: "SUPPORTED"})
	}
	writeJSON(w, &iamv1.QueryTestablePermissionsResponse{Permissions: out})
	return nil
}

var (
	roleIDRE     = regexp.MustCompile(`^[a-zA-Z0-9_.]{3,64}$`)
	permissionRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+$`)
)

func validateRoleFields(r *iamv1.Role) error {
	if len(r.Title) > 100 {
		return apierr.InvalidArgument("Role title must be at most 100 characters.")
	}
	if len(r.Description) > 300 {
		return apierr.InvalidArgument("Role description must be at most 300 characters.")
	}
	for _, p := range r.IncludedPermissions {
		if !permissionRE.MatchString(p) {
			return apierr.InvalidArgument("Permission %s is not valid.", p).WithReason(iamDomain, "INVALID_PERMISSION")
		}
	}
	switch r.Stage {
	case "", "ALPHA", "BETA", "GA", "DEPRECATED", "DISABLED", "EAP":
	default:
		return apierr.InvalidArgument("Invalid role stage %s.", r.Stage)
	}
	return nil
}

func (s *Service) createCustomRole(w http.ResponseWriter, r *http.Request, pid string) error {
	var req iamv1.CreateRoleRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := s.env.EnsureProject(pid); err != nil {
		return err
	}
	if err := s.check(r.Context(), "iam.roles.create", projectResource(pid)); err != nil {
		return err
	}
	role, err := s.newCustomRole(pid, req.RoleId, req.Role)
	if err != nil {
		return err
	}
	writeJSON(w, role)
	return nil
}

// newCustomRole creates projects/{pid}/roles/{id}.
func (s *Service) newCustomRole(pid, id string, in *iamv1.Role) (*iamv1.Role, error) {
	if !roleIDRE.MatchString(id) {
		return nil, apierr.InvalidArgument("The role ID %q is invalid. It must be 3 to 64 characters of letters, digits, underscores and periods.", id).WithReason(iamDomain, "INVALID_ROLE_ID")
	}
	if in == nil {
		in = &iamv1.Role{}
	}
	if err := validateRoleFields(in); err != nil {
		return nil, err
	}
	name := "projects/" + pid + "/roles/" + id
	role := &iamv1.Role{
		Name: name, Title: in.Title, Description: in.Description,
		IncludedPermissions: in.IncludedPermissions, Stage: in.Stage, Etag: s.newEtag(),
	}
	if role.Stage == "" {
		role.Stage = "ALPHA"
	}
	err := s.env.Store.Update(func(tx store.Tx) error {
		var cur iamv1.Role
		if store.GetJSON(tx, nsRoles, name, &cur) == nil {
			if cur.Deleted {
				return apierr.FailedPrecondition("You can't create a role with role_id (%s) where there is an existing role with that role_id in a deleted state.", id).WithReason(iamDomain, "ROLE_DELETED")
			}
			return apierr.AlreadyExists("A role named %s in projects/%s already exists.", id, pid).WithReason(iamDomain, "ROLE_ALREADY_EXISTS")
		}
		return store.PutJSON(tx, nsRoles, name, role)
	})
	return role, err
}

func (s *Service) customRole(ctx context.Context, pid, id, perm string) (*iamv1.Role, error) {
	if err := s.check(ctx, perm, projectResource(pid)); err != nil {
		return nil, err
	}
	var role iamv1.Role
	var found bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		found = store.GetJSON(tx, nsRoles, "projects/"+pid+"/roles/"+id, &role) == nil
		return nil
	})
	if !found {
		return nil, apierr.NotFound("The role named projects/%s/roles/%s was not found.", pid, id).WithReason(iamDomain, "ROLE_NOT_FOUND")
	}
	return &role, nil
}

func (s *Service) getCustomRole(w http.ResponseWriter, r *http.Request, pid, id string) error {
	role, err := s.customRole(r.Context(), pid, id, "iam.roles.get")
	if err != nil {
		return err
	}
	writeJSON(w, role)
	return nil
}

func (s *Service) listCustomRoles(w http.ResponseWriter, r *http.Request, pid string) error {
	if err := s.check(r.Context(), "iam.roles.list", projectResource(pid)); err != nil {
		return err
	}
	q := r.URL.Query()
	size, off, err := pageParams(r, 300, 1000)
	if err != nil {
		return err
	}
	showDeleted := q.Get("showDeleted") == "true"
	full := q.Get("view") == "FULL"
	var all []*iamv1.Role
	_ = s.env.Store.View(func(tx store.Tx) error {
		list, err := store.ListJSON[*iamv1.Role](tx, nsRoles, "projects/"+pid+"/roles/")
		for _, role := range list {
			if role.Deleted && !showDeleted {
				continue
			}
			if !full {
				role.IncludedPermissions = nil
			}
			all = append(all, role)
		}
		return err
	})
	items, next := page(all, size, off)
	writeJSON(w, &iamv1.ListRolesResponse{Roles: items, NextPageToken: next})
	return nil
}

// updateCustomRole applies fn to a stored role with etag checking.
func (s *Service) updateCustomRole(ctx context.Context, pid, id, perm, etag string, fn func(*iamv1.Role) error) (*iamv1.Role, error) {
	if _, err := s.customRole(ctx, pid, id, perm); err != nil {
		return nil, err
	}
	name := "projects/" + pid + "/roles/" + id
	var out iamv1.Role
	err := s.env.Store.Update(func(tx store.Tx) error {
		if err := store.GetJSON(tx, nsRoles, name, &out); err != nil {
			return apierr.NotFound("The role named %s was not found.", name)
		}
		if etag != "" && etag != out.Etag {
			return apierr.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.").WithReason(iamDomain, "ETAG_MISMATCH")
		}
		if err := fn(&out); err != nil {
			return err
		}
		out.Etag = s.newEtag()
		return store.PutJSON(tx, nsRoles, name, &out)
	})
	return &out, err
}

func (s *Service) patchCustomRole(w http.ResponseWriter, r *http.Request, pid, id string) error {
	var in iamv1.Role
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := validateRoleFields(&in); err != nil {
		return err
	}
	mask := r.URL.Query().Get("updateMask")
	role, err := s.updateCustomRole(r.Context(), pid, id, "iam.roles.update", in.Etag, func(role *iamv1.Role) error {
		if role.Deleted {
			return apierr.FailedPrecondition("You can't update a deleted role.").WithReason(iamDomain, "ROLE_DELETED")
		}
		fields := []string{"title", "description", "includedPermissions", "stage"}
		if mask != "" {
			fields = strings.Split(mask, ",")
		}
		for _, f := range fields {
			switch strings.TrimSpace(f) {
			case "title":
				role.Title = in.Title
			case "description":
				role.Description = in.Description
			case "includedPermissions", "included_permissions":
				role.IncludedPermissions = in.IncludedPermissions
			case "stage":
				if in.Stage != "" || mask != "" {
					role.Stage = in.Stage
				}
			default:
				return apierr.InvalidArgument("Invalid update mask field: %s", f)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, role)
	return nil
}

func (s *Service) deleteCustomRole(w http.ResponseWriter, r *http.Request, pid, id string) error {
	role, err := s.updateCustomRole(r.Context(), pid, id, "iam.roles.delete", r.URL.Query().Get("etag"), func(role *iamv1.Role) error {
		if role.Deleted {
			return apierr.FailedPrecondition("The role is already deleted.").WithReason(iamDomain, "ROLE_DELETED")
		}
		role.Deleted = true
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, role)
	return nil
}

func (s *Service) undeleteCustomRole(w http.ResponseWriter, r *http.Request, pid, id string) error {
	var req iamv1.UndeleteRoleRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	role, err := s.updateCustomRole(r.Context(), pid, id, "iam.roles.undelete", req.Etag, func(role *iamv1.Role) error {
		role.Deleted = false
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, role)
	return nil
}
