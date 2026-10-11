package sql

import (
	"context"
	"net/http"
	"slices"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Users (FR-SQL-002, FR-SQL-006). BUILT_IN users get passwords and, like
// in Cloud SQL, CREATEDB/CREATEROLE and membership in cloudsqlsuperuser.
// IAM users are LOGIN roles without a password, members of
// cloudsqliamuser / cloudsqliamserviceaccount, and authenticate through
// the agent with an access token (or the connector's ephemeral
// certificate). A service account's database user name is its email
// without ".gserviceaccount.com". A user's databaseRoles are roles it is
// granted; cloudsqlsuperuser among them also gives CREATEDB/CREATEROLE,
// which don't inherit through membership.

func newUser(project, inst, name, typ string) *sqladmin.User {
	u := &sqladmin.User{Kind: "sql#user", Name: name, Instance: inst, Project: project}
	if typ != "BUILT_IN" {
		u.Type = typ
	}
	switch typ {
	case "CLOUD_IAM_SERVICE_ACCOUNT":
		u.IamEmail = name + ".gserviceaccount.com"
		u.IamStatus = "ACTIVE"
	case "CLOUD_IAM_USER":
		u.IamEmail = name
		u.IamStatus = "ACTIVE"
	}
	u.Etag = etagOf(u)
	return u
}

// findUser looks a user up by name.
func (s *Service) findUser(project, inst, name string) (*userRecord, bool) {
	var u userRecord
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.GetJSON(tx, nsUsers, childKey(project, inst, name), &u) == nil && u.User != nil
		return nil
	})
	return &u, ok
}

// validateUser checks the user's name against its type.
func validateUser(u *sqladmin.User) error {
	if u.Name == "" {
		return errInvalid("User name is required.")
	}
	if len(u.Name) > 63 {
		return errInvalid("User name (%s) is too long; the maximum is 63 characters.", u.Name)
	}
	switch u.Type {
	case "", "BUILT_IN":
		if strings.HasPrefix(u.Name, "cloudsql") {
			return errInvalid("User name (%s) is reserved.", u.Name)
		}
	case "CLOUD_IAM_USER":
		if !strings.Contains(u.Name, "@") {
			return errInvalid("IAM user name (%s) must be an email address.", u.Name)
		}
	case "CLOUD_IAM_SERVICE_ACCOUNT":
		if strings.HasSuffix(u.Name, ".gserviceaccount.com") {
			return errInvalid("For a service account, the user name must be the email address without the .gserviceaccount.com domain suffix (%s).", strings.TrimSuffix(u.Name, ".gserviceaccount.com"))
		}
		if !strings.Contains(u.Name, "@") {
			return errInvalid("IAM service account user name (%s) must be the service account email without .gserviceaccount.com.", u.Name)
		}
	case "CLOUD_IAM_GROUP", "CLOUD_IAM_GROUP_USER", "CLOUD_IAM_GROUP_SERVICE_ACCOUNT", "CLOUD_IAM_WORKFORCE_IDENTITY", "ENTRAID_USER":
		return errUnsupported("User type " + u.Type)
	default:
		return errInvalid("Invalid user type %s.", u.Type)
	}
	return nil
}

// createRoleSQL returns the SQL creating u.
func createRoleSQL(u *sqladmin.User, password string) string {
	id := quoteIdent(u.Name)
	switch u.Type {
	case "CLOUD_IAM_USER":
		return "CREATE ROLE " + id + " WITH LOGIN;\nGRANT cloudsqliamuser TO " + id + ";\n"
	case "CLOUD_IAM_SERVICE_ACCOUNT":
		return "CREATE ROLE " + id + " WITH LOGIN;\nGRANT cloudsqliamserviceaccount TO " + id + ";\n"
	}
	q := "CREATE ROLE " + id + " WITH LOGIN CREATEDB CREATEROLE"
	if password != "" {
		q += " PASSWORD " + quoteLiteral(password)
	}
	return q + ";\nGRANT cloudsqlsuperuser TO " + id + ";\n"
}

// validateRoles checks a user's databaseRoles.
func validateRoles(roles []string) error {
	for _, r := range roles {
		switch {
		case r == "":
			return errInvalid("Database role name is required.")
		case r == "cloudsqladmin":
			return errInvalid("Database role (%s) is reserved.", r)
		}
	}
	return nil
}

// dedupe returns roles without repeats, in order.
func dedupe(roles []string) []string {
	var out []string
	for _, r := range roles {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// rolesSQL returns the SQL granting u the roles in add and revoking those
// in revoke. A BUILT_IN user is always a member of cloudsqlsuperuser.
func rolesSQL(u *sqladmin.User, add, revoke []string) string {
	id := quoteIdent(u.Name)
	var b strings.Builder
	for _, r := range add {
		if r == "cloudsqlsuperuser" {
			if u.Type == "" {
				continue
			}
			b.WriteString("ALTER ROLE " + id + " WITH CREATEDB CREATEROLE;\n")
		}
		b.WriteString("GRANT " + quoteIdent(r) + " TO " + id + ";\n")
	}
	for _, r := range revoke {
		if r == "cloudsqlsuperuser" {
			if u.Type == "" {
				continue
			}
			b.WriteString("ALTER ROLE " + id + " WITH NOCREATEDB NOCREATEROLE;\n")
		}
		b.WriteString("REVOKE " + quoteIdent(r) + " FROM " + id + ";\n")
	}
	return b.String()
}

func (s *Service) insertUser(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.users.create")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var u sqladmin.User
	if err := decode(r, &u, false); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := validateUser(&u); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := validateRoles(u.DatabaseRoles); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	project, inst := rec.Instance.Project, rec.Instance.Name
	if _, ok := s.findUser(project, inst, u.Name); ok {
		apierr.Write(w, apierr.AlreadyExists("Invalid request: user %q already exists.", u.Name).WithLegacy("alreadyExists"))
		return
	}
	typ := orDefault(u.Type, "BUILT_IN")
	ur := &userRecord{User: newUser(project, inst, u.Name, typ), Password: u.Password}
	ur.User.DatabaseRoles = dedupe(u.DatabaseRoles)
	ur.User.Etag = etagOf(ur.User)
	op := s.newOp(r.Context(), project, inst, "CREATE_USER")
	writeJSON(w, r, s.runOp(op, false, func(ctx context.Context) error {
		return s.createUser(ctx, project, inst, ur)
	}))
}

// createUser creates the role and records the user.
func (s *Service) createUser(ctx context.Context, project, inst string, ur *userRecord) error {
	q := createRoleSQL(ur.User, ur.Password) + rolesSQL(ur.User, ur.User.DatabaseRoles, nil)
	if _, err := s.execSQL(ctx, project, inst, "postgres", "", q); err != nil {
		return err
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsUsers, childKey(project, inst, ur.User.Name), ur)
	})
}

func (s *Service) listUsers(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.users.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	resp := &sqladmin.UsersListResponse{Kind: "sql#usersList"}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsUsers, rec.Instance.Project+"/"+rec.Instance.Name+"/", func(_ string, u *userRecord) {
			if u.User != nil {
				resp.Items = append(resp.Items, u.User)
			}
		})
		return nil
	})
	writeJSON(w, r, resp)
}

func (s *Service) getUser(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.users.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	u, ok := s.findUser(rec.Instance.Project, rec.Instance.Name, r.PathValue("name"))
	if !ok {
		apierr.Write(w, errUserNotFound(r.PathValue("name")))
		return
	}
	writeJSON(w, r, u.User)
}

func (s *Service) updateUser(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.users.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var body sqladmin.User
	if err := decode(r, &body, false); err != nil {
		apierr.Write(w, err)
		return
	}
	query := r.URL.Query()
	name := query.Get("name")
	if name == "" {
		name = body.Name
	}
	// Roles come from the query; the body's databaseRoles are ignored.
	roles := dedupe(query["databaseRoles"])
	if err := validateRoles(roles); err != nil {
		apierr.Write(w, err)
		return
	}
	revokeExisting := query.Get("revokeExistingRoles") == "true"
	project, inst := rec.Instance.Project, rec.Instance.Name
	ur, ok := s.findUser(project, inst, name)
	if !ok {
		apierr.Write(w, errUserNotFound(name))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	op := s.newOp(r.Context(), project, inst, "UPDATE_USER")
	writeJSON(w, r, s.runOp(op, false, func(ctx context.Context) error {
		if body.PasswordPolicy != nil {
			ur.User.PasswordPolicy = body.PasswordPolicy
		}
		password := ""
		if ur.User.Type == "" {
			password = body.Password
		}
		if err := s.updateRoles(ctx, project, inst, ur.User, roles, revokeExisting); err != nil {
			return err
		}
		return s.saveUser(ctx, project, inst, ur, password)
	}))
}

// updateRoles grants u the roles it lacks and, with revokeExisting,
// revokes those it has outside roles.
func (s *Service) updateRoles(ctx context.Context, project, inst string, u *sqladmin.User, roles []string, revokeExisting bool) error {
	var add, revoke []string
	for _, r := range roles {
		if !slices.Contains(u.DatabaseRoles, r) {
			add = append(add, r)
		}
	}
	keep := slices.Concat(u.DatabaseRoles, add)
	if revokeExisting {
		keep = roles
		for _, r := range u.DatabaseRoles {
			if !slices.Contains(roles, r) {
				revoke = append(revoke, r)
			}
		}
	}
	if q := rolesSQL(u, add, revoke); q != "" {
		if _, err := s.execSQL(ctx, project, inst, "postgres", "", q); err != nil {
			return err
		}
	}
	u.DatabaseRoles = keep
	return nil
}

// saveUser stores ur, first setting a built-in user's password in
// PostgreSQL when password is not empty.
func (s *Service) saveUser(ctx context.Context, project, inst string, ur *userRecord, password string) error {
	name := ur.User.Name
	if password != "" {
		q := "ALTER ROLE " + quoteIdent(name) + " WITH PASSWORD " + quoteLiteral(password) + ";"
		if _, err := s.execSQL(ctx, project, inst, "postgres", "", q); err != nil {
			return err
		}
		ur.Password = password
	}
	ur.User.Etag = etagOf(ur.User)
	return s.env.Store.Update(func(tx store.Tx) error {
		if name == "postgres" {
			if ir, ok := getInstance(tx, project, inst); ok {
				ir.RootPassword = ur.Password
				if err := store.PutJSON(tx, nsInstances, instKey(project, inst), ir); err != nil {
					return err
				}
			}
		}
		return store.PutJSON(tx, nsUsers, childKey(project, inst, name), ur)
	})
}

func (s *Service) deleteUser(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.users.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	name := r.URL.Query().Get("name")
	project, inst := rec.Instance.Project, rec.Instance.Name
	if _, ok := s.findUser(project, inst, name); !ok {
		apierr.Write(w, errUserNotFound(name))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	op := s.newOp(r.Context(), project, inst, "DELETE_USER")
	writeJSON(w, r, s.runOp(op, false, func(ctx context.Context) error {
		if _, err := s.execSQL(ctx, project, inst, "postgres", "", "DROP ROLE IF EXISTS "+quoteIdent(name)+";"); err != nil {
			return err
		}
		return s.env.Store.Update(func(tx store.Tx) error {
			return tx.Delete(nsUsers, childKey(project, inst, name))
		})
	}))
}
