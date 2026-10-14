package sql

import (
	"context"
	"reflect"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// applySeedYAML applies a "sql" section to s.
func applySeedYAML(t *testing.T, s *Service, section string) {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(section), &n); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySeed(context.Background(), n.Content[0], t.TempDir()); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

// exportSeedYAML returns s's exported "sql" section as YAML.
func exportSeedYAML(t *testing.T, s *Service) string {
	t.Helper()
	sec, err := s.ExportSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sec == nil {
		return ""
	}
	b, err := yaml.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExportSeedUnit checks FR-CORE-013 without a container runtime:
// stopped instances (activationPolicy NEVER, so no container) seeded
// with non-default settings export only those, and the export recreates
// the same instances on an empty Service.
func TestExportSeedUnit(t *testing.T) {
	s := testService(t, config.IAMOff)
	if got := exportSeedYAML(t, s); got != "" {
		t.Fatalf("empty export = %q", got)
	}
	applySeedYAML(t, s, `instances:
  - name: main
    project: my-project
    databaseVersion: POSTGRES_16
    region: europe-west1
    tier: db-custom-2-7680
    rootPassword: rootpw
    authorizedNetworks: [198.51.100.0/24]
    flags: {max_connections: "200", cloudsql.iam_authentication: "on"}
    settings:
      activationPolicy: NEVER
      edition: ENTERPRISE
      dataDiskSizeGb: "20"
      storageAutoResize: false
      userLabels: {team: web}
      locationPreference: {zone: europe-west1-c}
      ipConfiguration: {sslMode: ENCRYPTED_ONLY}
      backupConfiguration: {enabled: true, startTime: "03:00"}
  - name: bare
    project: my-project
    databaseVersion: POSTGRES_18
    settings: {activationPolicy: NEVER, availabilityType: REGIONAL}
`)
	// Databases and users need a running instance; write them directly.
	_ = s.env.Store.Update(func(tx store.Tx) error {
		_ = store.PutJSON(tx, nsDatabases, "my-project/main/app", newDatabase("my-project", "main", "app", "", ""))
		_ = store.PutJSON(tx, nsUsers, "my-project/main/app", &userRecord{User: newUser("my-project", "main", "app", "BUILT_IN"), Password: "apppw"})
		return store.PutJSON(tx, nsUsers, "my-project/main/sa@p.iam", &userRecord{User: newUser("my-project", "main", "sa@p.iam", "CLOUD_IAM_SERVICE_ACCOUNT")})
	})
	want := `instances:
    - name: bare
      project: my-project
      databaseVersion: POSTGRES_18
      settings:
        activationPolicy: NEVER
        availabilityType: REGIONAL
    - name: main
      project: my-project
      databaseVersion: POSTGRES_16
      region: europe-west1
      tier: db-custom-2-7680
      rootPassword: rootpw
      authorizedNetworks:
        - 198.51.100.0/24
      flags:
        cloudsql.iam_authentication: "on"
        max_connections: "200"
      settings:
        activationPolicy: NEVER
        backupConfiguration:
            enabled: true
            startTime: "03:00"
        dataDiskSizeGb: "20"
        edition: ENTERPRISE
        ipConfiguration:
            sslMode: ENCRYPTED_ONLY
        locationPreference:
            zone: europe-west1-c
        storageAutoResize: false
        userLabels:
            team: web
      databases:
        - app
      users:
        - name: app
          password: apppw
        - name: sa@p.iam
          type: CLOUD_IAM_SERVICE_ACCOUNT
`
	got := exportSeedYAML(t, s)
	if got != want {
		t.Fatalf("export =\n%s\nwant\n%s", got, want)
	}

	// Without the databases and users (they need a runtime), applying the
	// export to an empty Service exports the same again.
	var sec seedSection
	if err := yaml.Unmarshal([]byte(got), &sec); err != nil {
		t.Fatal(err)
	}
	for i := range sec.Instances {
		sec.Instances[i].Databases, sec.Instances[i].Users = nil, nil
	}
	b, _ := yaml.Marshal(sec)
	fresh := testService(t, config.IAMOff)
	applySeedYAML(t, fresh, string(b))
	if again := exportSeedYAML(t, fresh); again != string(b) {
		t.Fatalf("re-export =\n%s\nwant\n%s", again, b)
	}
}

// TestSettingsDiff checks that a boolean left out of the JSON because it
// is false is written when its default is true.
func TestSettingsDiff(t *testing.T) {
	got := settingsDiff(
		map[string]any{"ipConfiguration": map[string]any{"privateNetwork": "n"}, "tier": "x"},
		map[string]any{"ipConfiguration": map[string]any{"ipv4Enabled": true}, "tier": "x"})
	want := map[string]any{"ipConfiguration": map[string]any{"privateNetwork": "n", "ipv4Enabled": false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settingsDiff = %v, want %v", got, want)
	}
}

// TestExportSeedTierEdition checks that the tier is left out only when
// the seed would derive the same tier and edition without it.
func TestExportSeedTierEdition(t *testing.T) {
	cases := []struct{ version, tier, edition, wantTier, wantEdition string }{
		{"POSTGRES_18", "", "", "", ""},
		{"POSTGRES_15", "", "", "", ""},
		{"POSTGRES_15", "db-perf-optimized-N-2", "", "db-perf-optimized-N-2", ""},
		{"POSTGRES_18", "", "ENTERPRISE", "", "ENTERPRISE"},
		{"POSTGRES_18", "db-custom-4-16384", "", "db-custom-4-16384", ""},
	}
	for _, c := range cases {
		st := &sqladmin.Settings{Tier: c.tier, Edition: c.edition}
		applySettingsDefaults(st, map[string]any{}, "us-central1-b", c.version, true)
		var si seedInstance
		exportSettings(&si, &sqladmin.DatabaseInstance{Region: "us-central1", DatabaseVersion: c.version, Settings: st})
		edition, _ := si.Settings["edition"].(string)
		if si.Tier != c.wantTier || edition != c.wantEdition {
			t.Errorf("%s tier=%q edition=%q: exported tier=%q edition=%q settings=%v", c.version, c.tier, c.edition, si.Tier, edition, si.Settings)
		}
	}
}
