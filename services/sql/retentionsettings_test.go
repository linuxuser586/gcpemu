package sql

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// Backup retention settings (#1044): Cloud SQL's defaults by edition, its
// ranges, and transactionLogRetentionDays ≤ retainedBackups.
// https://docs.cloud.google.com/sql/docs/postgres/backup-recovery/backup-options

// getSettings returns the Cloud SQL instance's settings as the API
// reports them.
func getSettings(t *testing.T, s *Service, name string) *sqladmin.Settings {
	t.Helper()
	code, body := call(s, "GET", "/projects/proj-one/instances/"+name, "")
	if code != 200 {
		t.Fatalf("get %s: %d %s", name, code, body)
	}
	var in sqladmin.DatabaseInstance
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	return in.Settings
}

// retention returns the settings' retainedBackups and
// transactionLogRetentionDays.
func retention(st *sqladmin.Settings) (retained, logDays int64) {
	bc := st.BackupConfiguration
	return bc.BackupRetentionSettings.RetainedBackups, bc.TransactionLogRetentionDays
}

// insertErr inserts a Cloud SQL instance with settings and returns the
// 400's message, or "" when the insert succeeds.
func insertErr(t *testing.T, s *Service, name, version, settings string) string {
	t.Helper()
	code, body := call(s, "POST", "/projects/proj-one/instances",
		`{"name":"`+name+`","databaseVersion":"`+version+`","settings":`+settings+`}`)
	return rejection(t, name, code, body)
}

// rejection returns a 400 INVALID_ARGUMENT's message, or "" for a 200.
func rejection(t *testing.T, what string, code int, body string) string {
	t.Helper()
	switch code {
	case 200:
		return ""
	case 400:
		var e struct {
			Error struct {
				Message, Status string
				Errors          []struct{ Reason string }
			}
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error.Status != "INVALID_ARGUMENT" ||
			len(e.Error.Errors) != 1 || e.Error.Errors[0].Reason != "invalid" {
			t.Errorf("%s: 400 not INVALID_ARGUMENT with reason invalid: %s", what, body)
		}
		return e.Error.Message
	default:
		t.Fatalf("%s: %d %s", what, code, body)
		return ""
	}
}

func TestRetentionDefaultsByEdition(t *testing.T) {
	s, _ := fileService(t)
	for _, c := range []struct {
		name, version, settings string
		retained, logDays       int64
	}{
		{"ent", "POSTGRES_15", `{}`, 7, 7},
		{"plus-explicit", "POSTGRES_15", `{"edition":"ENTERPRISE_PLUS","tier":"db-perf-optimized-N-2"}`, 15, 14},
		{"plus-default", "POSTGRES_16", `{}`, 15, 14},
		{"ent-on-18", "POSTGRES_18", `{"edition":"ENTERPRISE","tier":"db-custom-1-3840"}`, 7, 7},
		{"unit-only", "POSTGRES_18", `{"backupConfiguration":{"backupRetentionSettings":{"retentionUnit":"COUNT"}}}`, 15, 14},
		{"zero", "POSTGRES_15", `{"backupConfiguration":{"backupRetentionSettings":{"retentionUnit":"COUNT","retainedBackups":0},"transactionLogRetentionDays":0}}`, 7, 7},
	} {
		if msg := insertErr(t, s, c.name, c.version, c.settings); msg != "" {
			t.Errorf("%s: insert rejected: %s", c.name, msg)
			continue
		}
		if r, d := retention(getSettings(t, s, c.name)); r != c.retained || d != c.logDays {
			t.Errorf("%s: retainedBackups=%d transactionLogRetentionDays=%d, want %d and %d", c.name, r, d, c.retained, c.logDays)
		}
	}
}

// retentionSettingsJSON is a settings object on edition, with a tier of
// that edition, keeping retained backups and logDays of logs; extra is
// more backupConfiguration fields, each followed by a comma.
func retentionSettingsJSON(edition string, retained, logDays int64, extra string) string {
	tier := "db-custom-1-3840"
	if edition == "ENTERPRISE_PLUS" {
		tier = "db-perf-optimized-N-2"
	}
	return `{"edition":"` + edition + `","tier":"` + tier + `","backupConfiguration":{` + extra +
		`"transactionLogRetentionDays":` + strconv.FormatInt(logDays, 10) +
		`,"backupRetentionSettings":{"retentionUnit":"COUNT","retainedBackups":` + strconv.FormatInt(retained, 10) + `}}}`
}

func TestRetentionChecksOnInsert(t *testing.T) {
	s, _ := fileService(t)
	const ent, plus = "ENTERPRISE", "ENTERPRISE_PLUS"
	for i, c := range []struct {
		edition           string
		retained, logDays int64
		extra             string
		msg               string // "" accepts
	}{
		{ent, 1, 7, "", "transactionLogRetentionDays (7) must be less than or equal to backupRetentionSettings.retainedBackups (1)."},
		{ent, 7, 7, "", ""},
		{ent, 30, 8, "", "transactionLogRetentionDays (8) must be between 1 and 7 for the ENTERPRISE edition."},
		{plus, 8, 8, "", ""},
		{plus, 40, 36, "", "transactionLogRetentionDays (36) must be between 1 and 35 for the ENTERPRISE_PLUS edition."},
		{plus, 35, 35, "", ""},
		{ent, 366, 1, "", "backupRetentionSettings.retainedBackups (366) must be between 1 and 365."},
		{ent, 365, 1, "", ""},
		{ent, 7, -1, "", "transactionLogRetentionDays (-1) must be between 1 and 7 for the ENTERPRISE edition."},
		{ent, -1, 1, "", "backupRetentionSettings.retainedBackups (-1) must be between 1 and 365."},
		// The range error wins over the relation.
		{ent, 2, 8, "", "transactionLogRetentionDays (8) must be between 1 and 7 for the ENTERPRISE edition."},
		// The checks apply with backups and PITR off.
		{ent, 1, 7, `"enabled":false,"pointInTimeRecoveryEnabled":false,`, "transactionLogRetentionDays (7) must be less than or equal to backupRetentionSettings.retainedBackups (1)."},
	} {
		msg := insertErr(t, s, "r"+strconv.Itoa(i), "POSTGRES_18", retentionSettingsJSON(c.edition, c.retained, c.logDays, c.extra))
		if want := map[bool]string{true: "Invalid request: " + c.msg}[c.msg != ""]; msg != want {
			t.Errorf("%s retained=%d logDays=%d: got %q, want %q", c.edition, c.retained, c.logDays, msg, want)
		}
	}
}

// putPlusInstance stores a store-only Enterprise Plus Cloud SQL instance,
// defaulted as an insert would, whose retention is then set to retained
// and logDays when they are not 0.
func putPlusInstance(t *testing.T, s *Service, name string, retained, logDays int64) {
	t.Helper()
	putInstance(t, s, "proj-one", name, false)
	if err := s.updateRecord("proj-one", name, func(r *instanceRecord) error {
		applySettingsDefaults(r.Instance.Settings, nil, "", r.Instance.DatabaseVersion, true)
		r.Instance.Settings.IpConfiguration.Ipv4Enabled = true
		if retained != 0 {
			r.Instance.Settings.BackupConfiguration.BackupRetentionSettings.RetainedBackups = retained
			r.Instance.Settings.BackupConfiguration.TransactionLogRetentionDays = logDays
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// modify sends a PATCH or PUT of the Cloud SQL instance's settings and
// returns the 400's message, or "" once an accepted request's Operation is
// done. patchSettings fails the test on a rejection instead.
func modify(t *testing.T, s *Service, method, name, settings string) string {
	t.Helper()
	code, body := call(s, method, "/projects/proj-one/instances/"+name, `{"settings":`+settings+`}`)
	msg := rejection(t, method+" "+name, code, body)
	if code == 200 {
		if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
			t.Fatalf("%s %s %s: %+v", method, name, settings, op.Error.Errors[0])
		}
	}
	return msg
}

func TestRetentionChecksOnPatchAndUpdate(t *testing.T) {
	s, _ := fileService(t)
	const plusTier = `"tier":"db-perf-optimized-N-2","ipConfiguration":{"ipv4Enabled":true},`

	// Lowering only retainedBackups below the stored log days is rejected
	// by a patch and by an update, and the stored Cloud SQL instance is
	// unchanged.
	putPlusInstance(t, s, "lower", 15, 7)
	for _, method := range []string{"PATCH", "PUT"} {
		msg := modify(t, s, method, "lower", `{`+plusTier+`"backupConfiguration":{"backupRetentionSettings":{"retainedBackups":5}}}`)
		if want := "Invalid request: transactionLogRetentionDays (7) must be less than or equal to backupRetentionSettings.retainedBackups (5)."; msg != want {
			t.Errorf("%s lowering retainedBackups: %q, want %q", method, msg, want)
		}
		if r, d := retention(getSettings(t, s, "lower")); r != 15 || d != 7 {
			t.Errorf("after the rejected %s: %d/%d, want 15/7", method, r, d)
		}
	}

	// An edition change that takes the stored log days out of range is
	// rejected, not clamped; fixing them in the same patch is accepted.
	putPlusInstance(t, s, "edition", 0, 0)
	msg := modify(t, s, "PATCH", "edition", `{"edition":"ENTERPRISE","tier":"db-custom-1-3840"}`)
	if want := "Invalid request: transactionLogRetentionDays (14) must be between 1 and 7 for the ENTERPRISE edition."; msg != want {
		t.Errorf("edition change: %q, want %q", msg, want)
	}
	if msg := modify(t, s, "PATCH", "edition", `{"edition":"ENTERPRISE","tier":"db-custom-1-3840","backupConfiguration":{"transactionLogRetentionDays":7}}`); msg != "" {
		t.Errorf("edition change with log days 7: %s", msg)
	}
	if r, d := retention(getSettings(t, s, "edition")); r != 15 || d != 7 {
		t.Errorf("after edition change: %d/%d, want 15/7", r, d)
	}

	// An Enterprise Plus Cloud SQL instance stored with 7 and 7 is not
	// rewritten to the edition's defaults, on load or by an unrelated patch.
	putPlusInstance(t, s, "old", 7, 7)
	if r, d := retention(getSettings(t, s, "old")); r != 7 || d != 7 {
		t.Errorf("stored 7/7 loads as %d/%d", r, d)
	}
	if msg := modify(t, s, "PATCH", "old", `{"userLabels":{"a":"b"}}`); msg != "" {
		t.Errorf("unrelated patch: %s", msg)
	}
	if r, d := retention(getSettings(t, s, "old")); r != 7 || d != 7 {
		t.Errorf("stored 7/7 after an unrelated patch: %d/%d", r, d)
	}

	// A Cloud SQL instance stored with an invalid combination fails any
	// patch until its retention is fixed.
	putPlusInstance(t, s, "bad", 1, 7)
	if msg := modify(t, s, "PATCH", "bad", `{"userLabels":{"a":"b"}}`); !strings.Contains(msg, "retainedBackups (1)") {
		t.Errorf("unrelated patch of a stored invalid combination: %q", msg)
	}
	if msg := modify(t, s, "PATCH", "bad", `{"backupConfiguration":{"backupRetentionSettings":{"retainedBackups":7}}}`); msg != "" {
		t.Errorf("fixing patch: %s", msg)
	}
}

// TestExportSeedLeavesOutDefaultRetention checks that an exported Seed
// file writes backup retention only when it is not the default of the
// Cloud SQL instance's edition.
func TestExportSeedLeavesOutDefaultRetention(t *testing.T) {
	for _, c := range []struct {
		version, edition  string
		retained, logDays int64 // 0: the default
		want              bool
	}{
		{"POSTGRES_18", "", 0, 0, false},
		{"POSTGRES_15", "", 0, 0, false},
		{"POSTGRES_18", "ENTERPRISE", 0, 0, false},
		{"POSTGRES_18", "", 7, 7, true},
	} {
		st := &sqladmin.Settings{Edition: c.edition}
		if c.edition == "ENTERPRISE" {
			st.Tier = "db-custom-1-3840"
		}
		applySettingsDefaults(st, map[string]any{}, "us-central1-b", c.version, true)
		if c.retained != 0 {
			st.BackupConfiguration.BackupRetentionSettings.RetainedBackups = c.retained
			st.BackupConfiguration.TransactionLogRetentionDays = c.logDays
		}
		var si seedInstance
		exportSettings(&si, &sqladmin.DatabaseInstance{Region: "us-central1", DatabaseVersion: c.version, Settings: st})
		if _, got := si.Settings["backupConfiguration"]; got != c.want {
			t.Errorf("%s edition=%q %d/%d: backupConfiguration exported %t, want %t: %v", c.version, c.edition, c.retained, c.logDays, got, c.want, si.Settings)
		}
	}
}
