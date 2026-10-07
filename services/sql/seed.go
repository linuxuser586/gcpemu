package sql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"gopkg.in/yaml.v3"
)

// Seeding (FR-CORE-011): SQL instances with databases, users and init
// SQL. Applying a seed is idempotent: existing instances, databases and
// users are kept, and init SQL runs again only when its content changes
// (write it idempotently, e.g. CREATE TABLE IF NOT EXISTS).
//
//	sql:
//	  instances:
//	    - name: main
//	      project: my-project
//	      databaseVersion: POSTGRES_16      # default POSTGRES_17
//	      region: us-central1
//	      tier: db-custom-1-3840
//	      rootPassword: secret
//	      authorizedNetworks: [0.0.0.0/0]
//	      flags: {cloudsql.iam_authentication: "on"}
//	      settings: {...}                   # any other settings fields
//	      databases: [app]
//	      users:
//	        - {name: app, password: pw}
//	        - {name: sa@my-project.iam, type: CLOUD_IAM_SERVICE_ACCOUNT}
//	      initSQL:
//	        - {database: app, file: ./schema.sql}
//	        - {database: app, user: app, sql: "INSERT INTO t VALUES (1) ON CONFLICT DO NOTHING"}

type seedSection struct {
	Instances []seedInstance `yaml:"instances"`
}

type seedInstance struct {
	Name               string            `yaml:"name"`
	Project            string            `yaml:"project"`
	DatabaseVersion    string            `yaml:"databaseVersion"`
	Region             string            `yaml:"region"`
	Tier               string            `yaml:"tier"`
	RootPassword       string            `yaml:"rootPassword"`
	AuthorizedNetworks []string          `yaml:"authorizedNetworks"`
	Flags              map[string]string `yaml:"flags"`
	Settings           map[string]any    `yaml:"settings"`
	Databases          []string          `yaml:"databases"`
	Users              []seedUser        `yaml:"users"`
	InitSQL            []seedSQL         `yaml:"initSQL"`
}

type seedUser struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
	Type     string `yaml:"type"`
}

type seedSQL struct {
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	File     string `yaml:"file"`
	SQL      string `yaml:"sql"`
}

// ApplySeed implements emu.Seeder.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var sec seedSection
	if err := section.Decode(&sec); err != nil {
		return err
	}
	ctx = system(ctx)
	for i, in := range sec.Instances {
		if err := s.seedInstance(ctx, in, baseDir); err != nil {
			return fmt.Errorf("instances[%d] (%s): %w", i, in.Name, err)
		}
	}
	return nil
}

func (s *Service) seedInstance(ctx context.Context, si seedInstance, baseDir string) error {
	project := si.Project
	if project == "" && len(s.env.Config.Projects) > 0 {
		project = s.env.Config.Projects[0]
	}
	if project == "" {
		return errors.New("project is required")
	}
	if err := s.env.EnsureProject(project); err != nil {
		return err
	}
	// Resolve init SQL first so that a missing file fails fast.
	var scripts []seedSQL
	h := sha256.New()
	for _, q := range si.InitSQL {
		if q.File != "" {
			p := q.File
			if !filepath.IsAbs(p) {
				p = filepath.Join(baseDir, p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			q.SQL = string(b) + "\n" + q.SQL
		}
		q.Database = orDefault(q.Database, "postgres")
		scripts = append(scripts, q)
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", q.Database, q.User, q.SQL)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	if _, err := s.loadRecord(project, si.Name); err != nil {
		settings := map[string]any{}
		for k, v := range si.Settings {
			settings[k] = v
		}
		if si.Tier != "" {
			settings["tier"] = si.Tier
		}
		if len(si.AuthorizedNetworks) > 0 {
			ip, _ := settings["ipConfiguration"].(map[string]any)
			if ip == nil {
				ip = map[string]any{}
			}
			var nets []map[string]any
			for _, n := range si.AuthorizedNetworks {
				nets = append(nets, map[string]any{"value": n})
			}
			ip["authorizedNetworks"] = nets
			settings["ipConfiguration"] = ip
		}
		if len(si.Flags) > 0 {
			var flags []map[string]any
			for _, k := range sortedKeys(si.Flags) {
				flags = append(flags, map[string]any{"name": k, "value": si.Flags[k]})
			}
			settings["databaseFlags"] = flags
		}
		raw := map[string]any{
			"name": si.Name, "databaseVersion": si.DatabaseVersion, "region": si.Region,
			"rootPassword": si.RootPassword, "settings": settings,
		}
		// YAML maps decode as map[string]any already; normalise via JSON.
		b, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		raw = map[string]any{}
		_ = json.Unmarshal(b, &raw)
		op, err := s.createInstance(ctx, project, raw, "CREATE", nil)
		if err != nil {
			return err
		}
		if err := s.awaitOp(ctx, op); err != nil {
			return err
		}
	}
	rec, err := s.loadRecord(project, si.Name)
	if err != nil {
		return err
	}
	if err := requireRunnable(rec); err != nil {
		if len(si.Databases)+len(si.Users)+len(scripts) == 0 {
			return nil
		}
		return fmt.Errorf("instance is not running: %w", err)
	}
	for _, d := range si.Databases {
		if s.databaseExists(project, si.Name, d) {
			continue
		}
		if err := s.withLock(ctx, project, si.Name, func(ctx context.Context) error {
			return s.createDatabase(ctx, project, si.Name, newDatabase(project, si.Name, d, "", ""))
		}); err != nil {
			return fmt.Errorf("database %s: %w", d, err)
		}
	}
	for _, u := range si.Users {
		if _, ok := s.findUser(project, si.Name, u.Name); ok {
			continue
		}
		user := &sqladmin.User{Name: u.Name, Type: u.Type}
		if err := validateUser(user); err != nil {
			return fmt.Errorf("user %s: %w", u.Name, err)
		}
		ur := &userRecord{User: newUser(project, si.Name, u.Name, orDefault(u.Type, "BUILT_IN")), Password: u.Password}
		if err := s.withLock(ctx, project, si.Name, func(ctx context.Context) error {
			return s.createUser(ctx, project, si.Name, ur)
		}); err != nil {
			return fmt.Errorf("user %s: %w", u.Name, err)
		}
	}
	if len(scripts) > 0 && rec.SeedHash != hash {
		for i, q := range scripts {
			if _, err := s.execSQL(ctx, project, si.Name, q.Database, q.User, q.SQL); err != nil {
				return fmt.Errorf("initSQL[%d]: %w", i, err)
			}
		}
		return s.updateRecord(project, si.Name, func(r *instanceRecord) error {
			r.SeedHash = hash
			return nil
		})
	}
	return nil
}

// withLock runs fn under the instance lock.
func (s *Service) withLock(ctx context.Context, project, name string, fn func(ctx context.Context) error) error {
	mu := s.lock(project, name)
	defer mu.Unlock()
	return fn(ctx)
}

// awaitOp waits for an operation started by this process.
func (s *Service) awaitOp(ctx context.Context, op *sqladmin.Operation) error {
	for {
		cur, ok := s.loadOp(op.TargetProject, op.Name)
		if !ok {
			return fmt.Errorf("operation %s vanished", op.Name)
		}
		if cur.Status == "DONE" {
			if cur.Error != nil && len(cur.Error.Errors) > 0 {
				return fmt.Errorf("%s: %s", cur.Error.Errors[0].Code, cur.Error.Errors[0].Message)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
