package sql

import (
	"context"
	"net/http"
	"regexp"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Databases (FR-SQL-002). Databases created through the API are owned by
// cloudsqlsuperuser, as in Cloud SQL.

const (
	defaultCharset   = "UTF8"
	defaultCollation = "en_US.UTF8"
)

var dbNameRe = regexp.MustCompile(`^[^\x00]{1,63}$`)

func newDatabase(project, inst, name, charset, collation string) *sqladmin.Database {
	d := &sqladmin.Database{
		Kind:      "sql#database",
		Name:      name,
		Charset:   orDefault(charset, defaultCharset),
		Collation: orDefault(collation, defaultCollation),
		Instance:  inst,
		Project:   project,
		SelfLink:  instanceLink(project, inst) + "/databases/" + name,
	}
	d.Etag = etagOf(d)
	return d
}

// instanceFor loads the instance for a child-resource call after checking
// permission on it.
func (s *Service) instanceFor(r *http.Request, perm string) (*instanceRecord, error) {
	project, name := r.PathValue("project"), r.PathValue("instance")
	if err := s.check(r.Context(), perm, instanceResource(project, name)); err != nil {
		return nil, err
	}
	return s.loadRecord(project, name)
}

func (s *Service) insertDatabase(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.databases.create")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var d sqladmin.Database
	if err := decode(r, &d, false); err != nil {
		apierr.Write(w, err)
		return
	}
	if !dbNameRe.MatchString(d.Name) {
		apierr.Write(w, errInvalid("Invalid database name (%s).", d.Name))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	project, inst := rec.Instance.Project, rec.Instance.Name
	if s.databaseExists(project, inst, d.Name) {
		apierr.Write(w, apierr.AlreadyExists("Invalid request: database %q already exists.", d.Name).WithLegacy("alreadyExists"))
		return
	}
	db := newDatabase(project, inst, d.Name, d.Charset, d.Collation)
	op := s.newOp(r.Context(), project, inst, "CREATE_DATABASE")
	writeJSON(w, r, s.runOp(op, false, func(ctx context.Context) error {
		return s.createDatabase(ctx, project, inst, db)
	}))
}

// createDatabase creates the database in PostgreSQL and records it.
func (s *Service) createDatabase(ctx context.Context, project, inst string, db *sqladmin.Database) error {
	q := "CREATE DATABASE " + quoteIdent(db.Name) + " WITH OWNER cloudsqlsuperuser TEMPLATE template0 ENCODING " + quoteLiteral(db.Charset)
	if db.Collation != defaultCollation {
		q += " LC_COLLATE " + quoteLiteral(db.Collation) + " LC_CTYPE " + quoteLiteral(db.Collation)
	}
	if _, err := s.execSQL(ctx, project, inst, "postgres", "", q+";"); err != nil {
		return err
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsDatabases, childKey(project, inst, db.Name), db)
	})
}

func (s *Service) databaseExists(project, inst, name string) bool {
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.Exists(tx, nsDatabases, childKey(project, inst, name))
		return nil
	})
	return ok
}

func (s *Service) loadDatabase(project, inst, name string) (*sqladmin.Database, bool) {
	var d sqladmin.Database
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.GetJSON(tx, nsDatabases, childKey(project, inst, name), &d) == nil
		return nil
	})
	return &d, ok
}

func (s *Service) listDatabases(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.databases.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	resp := &sqladmin.DatabasesListResponse{Kind: "sql#databasesList"}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsDatabases, rec.Instance.Project+"/"+rec.Instance.Name+"/", func(_ string, d *sqladmin.Database) {
			resp.Items = append(resp.Items, d)
		})
		return nil
	})
	writeJSON(w, r, resp)
}

func (s *Service) getDatabase(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.databases.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	d, ok := s.loadDatabase(rec.Instance.Project, rec.Instance.Name, r.PathValue("database"))
	if !ok {
		apierr.Write(w, errDatabaseNotFound(r.PathValue("database")))
		return
	}
	writeJSON(w, r, d)
}

// patchDatabase handles patch and update. PostgreSQL cannot change a
// database's encoding or collation, so only an unchanged resource (or a
// rename-free no-op) is accepted.
func (s *Service) patchDatabase(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.databases.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	project, inst, name := rec.Instance.Project, rec.Instance.Name, r.PathValue("database")
	cur, ok := s.loadDatabase(project, inst, name)
	if !ok {
		apierr.Write(w, errDatabaseNotFound(name))
		return
	}
	var d sqladmin.Database
	if err := decode(r, &d, false); err != nil {
		apierr.Write(w, err)
		return
	}
	if (d.Name != "" && d.Name != name) || (d.Charset != "" && d.Charset != cur.Charset) || (d.Collation != "" && d.Collation != cur.Collation) {
		apierr.Write(w, errInvalid("The charset, collation and name of a PostgreSQL database cannot be changed."))
		return
	}
	op := s.newOp(r.Context(), project, inst, "UPDATE_DATABASE")
	writeJSON(w, r, s.runOp(op, false, func(context.Context) error { return nil }))
}

func (s *Service) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.databases.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	project, inst, name := rec.Instance.Project, rec.Instance.Name, r.PathValue("database")
	if !s.databaseExists(project, inst, name) {
		apierr.Write(w, errDatabaseNotFound(name))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	op := s.newOp(r.Context(), project, inst, "DELETE_DATABASE")
	writeJSON(w, r, s.runOp(op, false, func(ctx context.Context) error {
		if _, err := s.execSQL(ctx, project, inst, "postgres", "", "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE);"); err != nil {
			return err
		}
		return s.env.Store.Update(func(tx store.Tx) error {
			return tx.Delete(nsDatabases, childKey(project, inst, name))
		})
	}))
}
