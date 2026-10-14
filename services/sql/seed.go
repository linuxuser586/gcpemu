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
	"reflect"
	"sort"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/emu"
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
//	      databaseVersion: POSTGRES_16      # default POSTGRES_18
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
	Project            string            `yaml:"project,omitempty"`
	DatabaseVersion    string            `yaml:"databaseVersion,omitempty"`
	Region             string            `yaml:"region,omitempty"`
	Tier               string            `yaml:"tier,omitempty"`
	RootPassword       string            `yaml:"rootPassword,omitempty"`
	AuthorizedNetworks []string          `yaml:"authorizedNetworks,omitempty"`
	Flags              map[string]string `yaml:"flags,omitempty"`
	Settings           map[string]any    `yaml:"settings,omitempty"`
	Databases          []string          `yaml:"databases,omitempty"`
	Users              []seedUser        `yaml:"users,omitempty"`
	InitSQL            []seedSQL         `yaml:"initSQL,omitempty"`
}

type seedUser struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password,omitempty"`
	Type     string `yaml:"type,omitempty"`
}

type seedSQL struct {
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	File     string `yaml:"file"`
	SQL      string `yaml:"sql"`
}

var _ emu.SeedExporter = (*Service)(nil)

// ExportSeed writes the instances back as the "sql" section (FR-CORE-013),
// ordered by project and name: each instance's settings that differ from
// the create defaults, its root password, and the databases and users
// beyond the built-in postgres ones, with built-in users' passwords. The
// rows in the databases are data plane and are not exported (no initSQL),
// nor are a database's charset and collation or a user's databaseRoles,
// which the seed file cannot declare. databaseVersion is always written,
// since its default moves with releases.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	recs := s.instanceRecords()
	if len(recs) == 0 {
		return nil, nil
	}
	sort.Slice(recs, func(i, j int) bool {
		a, b := recs[i].Instance, recs[j].Instance
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Name < b.Name
	})
	var sec seedSection
	for _, rec := range recs {
		in := rec.Instance
		si := seedInstance{Name: in.Name, Project: in.Project, DatabaseVersion: in.DatabaseVersion, RootPassword: rec.RootPassword}
		if in.Region != defaultRegion {
			si.Region = in.Region
		}
		exportSettings(&si, in)
		dbs, users := s.snapshotAPIState(in.Project, in.Name)
		for _, d := range dbs {
			if d.Name != "postgres" {
				si.Databases = append(si.Databases, d.Name)
			}
		}
		for _, u := range users {
			if u.User == nil || u.User.Name == "postgres" {
				continue
			}
			su := seedUser{Name: u.User.Name, Type: u.User.Type}
			if su.Type == "" || su.Type == "BUILT_IN" {
				su.Type, su.Password = "", u.Password
			}
			si.Users = append(si.Users, su)
		}
		sec.Instances = append(sec.Instances, si)
	}
	return sec, nil
}

// exportSettings fills si's tier, flags, authorized networks and settings
// with in's settings that applying the seed would not default to.
func exportSettings(si *seedInstance, in *sqladmin.DatabaseInstance) {
	cur := in.Settings
	if cur == nil {
		return
	}
	// The defaults of an instance created with the same tier, so that an
	// edition derived from the tier is not written.
	def := &sqladmin.Settings{Tier: cur.Tier}
	applySettingsDefaults(def, map[string]any{}, in.Region+"-b", in.DatabaseVersion, true)
	st := settingsDiff(toMap(cur), toMap(def))
	for _, k := range []string{"tier", "settingsVersion", "databaseFlags"} {
		delete(st, k)
	}
	// The tier is written unless leaving it out derives the same tier and
	// edition.
	bare := &sqladmin.Settings{}
	if e, ok := st["edition"].(string); ok {
		bare.Edition = e
	}
	applySettingsDefaults(bare, map[string]any{}, in.Region+"-b", in.DatabaseVersion, true)
	if bare.Tier != cur.Tier || bare.Edition != cur.Edition {
		si.Tier = cur.Tier
	}
	if len(cur.DatabaseFlags) > 0 {
		si.Flags = flagMap(cur)
	}
	if ip := child(st, "ipConfiguration"); ip != nil {
		if _, ok := ip["sslMode"]; ok {
			delete(ip, "requireSsl") // derived from sslMode
		}
		nets := cur.IpConfiguration.AuthorizedNetworks
		plain := true
		for _, n := range nets {
			plain = plain && n != nil && n.Name == "" && n.ExpirationTime == ""
		}
		if plain {
			delete(ip, "authorizedNetworks")
			for _, n := range nets {
				si.AuthorizedNetworks = append(si.AuthorizedNetworks, n.Value)
			}
		} else if list, ok := ip["authorizedNetworks"].([]any); ok {
			for _, n := range list {
				if m, ok := n.(map[string]any); ok {
					delete(m, "kind")
				}
			}
		}
		if len(ip) == 0 {
			delete(st, "ipConfiguration")
		}
	}
	if len(st) > 0 {
		si.Settings = st
	}
}

// settingsDiff returns the fields of cur (a settings object as JSON) that
// differ from def, recursing into objects. A boolean def has that cur
// leaves out (false, omitted from the JSON) is written as false.
func settingsDiff(cur, def map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range cur {
		dv, ok := def[k]
		if m, isMap := v.(map[string]any); isMap {
			dm, _ := dv.(map[string]any)
			if d := settingsDiff(m, dm); len(d) > 0 {
				out[k] = d
			}
			continue
		}
		if !ok || !reflect.DeepEqual(v, dv) {
			out[k] = v
		}
	}
	for k, dv := range def {
		if _, ok := cur[k]; !ok {
			if _, isBool := dv.(bool); isBool {
				out[k] = false
			}
		}
	}
	return out
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

	var after func(ctx context.Context) error
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
		// A stopped (NEVER) instance gets its contents while the
		// container runs once during creation, as a clone does.
		if settings["activationPolicy"] == "NEVER" && len(si.Databases)+len(si.Users)+len(scripts) > 0 {
			after = func(ctx context.Context) error { return s.seedContents(ctx, project, si, scripts, hash, false) }
		}
		op, err := s.createInstance(ctx, project, raw, "CREATE", after)
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
		if after != nil || len(si.Databases)+len(si.Users)+len(scripts) == 0 {
			return nil
		}
		return fmt.Errorf("instance is not running: %w", err)
	}
	return s.seedContents(ctx, project, si, scripts, hash, true)
}

// seedContents creates the seeded databases and users that are missing
// and runs the init SQL when its hash changed. lock is false inside an
// operation, which already holds the instance lock.
func (s *Service) seedContents(ctx context.Context, project string, si seedInstance, scripts []seedSQL, hash string, lock bool) error {
	run := func(fn func(ctx context.Context) error) error {
		if lock {
			return s.withLock(ctx, project, si.Name, fn)
		}
		return fn(ctx)
	}
	for _, d := range si.Databases {
		if s.databaseExists(project, si.Name, d) {
			continue
		}
		if err := run(func(ctx context.Context) error {
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
		if err := run(func(ctx context.Context) error {
			return s.createUser(ctx, project, si.Name, ur)
		}); err != nil {
			return fmt.Errorf("user %s: %w", u.Name, err)
		}
	}
	rec, err := s.loadRecord(project, si.Name)
	if err != nil {
		return err
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
