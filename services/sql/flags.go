package sql

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Database flags (FR-SQL-003): an embedded subset of Cloud SQL's
// PostgreSQL allow-list with types, ranges and restart requirements.
// Flags are validated on insert/patch/update and applied with ALTER SYSTEM
// (reload, or a restart within the same operation for restart-required
// flags). Flags in the cloudsql.* namespace and extension settings are
// Recorded only, except cloudsql.logical_decoding (wal_level=logical).

type flagDef struct {
	Type    string // BOOLEAN, INTEGER, FLOAT, STRING, REPEATED_STRING
	Min     float64
	Max     float64
	Values  []string
	Restart bool
	Since   int // first major version (0 = all)
	// Recorded flags are stored and reported but not applied.
	Recorded bool
}

const maxInt = 2147483647

func intFlag(min, max float64, restart bool) flagDef {
	return flagDef{Type: "INTEGER", Min: min, Max: max, Restart: restart}
}
func floatFlag(min, max float64) flagDef { return flagDef{Type: "FLOAT", Min: min, Max: max} }
func boolFlag(restart bool) flagDef      { return flagDef{Type: "BOOLEAN", Restart: restart} }
func enumFlag(vals ...string) flagDef    { return flagDef{Type: "STRING", Values: vals} }
func strFlag() flagDef                   { return flagDef{Type: "STRING"} }
func recorded(f flagDef) flagDef         { f.Recorded = true; return f }

var logLevels = []string{"debug5", "debug4", "debug3", "debug2", "debug1", "info", "notice", "warning", "error", "log", "fatal", "panic"}

var flagDefs = map[string]flagDef{
	// Connections and memory.
	"max_connections":                  intFlag(14, 262143, true),
	"max_locks_per_transaction":        intFlag(10, maxInt, true),
	"max_pred_locks_per_transaction":   intFlag(10, maxInt, true),
	"max_prepared_transactions":        intFlag(0, 262143, true),
	"max_worker_processes":             intFlag(8, 262143, true),
	"max_parallel_workers":             intFlag(0, 1024, false),
	"max_parallel_workers_per_gather":  intFlag(0, 1024, false),
	"max_parallel_maintenance_workers": intFlag(0, 1024, false),
	"max_files_per_process":            intFlag(64, maxInt, true),
	"max_stack_depth":                  intFlag(100, 7680, false),
	"shared_buffers":                   intFlag(1600, maxInt, true),
	"work_mem":                         intFlag(64, maxInt, false),
	"maintenance_work_mem":             intFlag(1024, maxInt, false),
	"autovacuum_work_mem":              intFlag(-1, maxInt, false),
	"temp_buffers":                     intFlag(100, 1073741823, false),
	"effective_cache_size":             intFlag(1, maxInt, false),
	"effective_io_concurrency":         intFlag(0, 1000, false),
	"hash_mem_multiplier":              floatFlag(1, 1000),
	"temp_file_limit":                  intFlag(-1, maxInt, false),
	"track_activity_query_size":        intFlag(100, 1048576, true),
	"tcp_keepalives_idle":              intFlag(0, maxInt, false),
	"tcp_keepalives_interval":          intFlag(0, maxInt, false),
	"tcp_keepalives_count":             intFlag(0, maxInt, false),
	"password_encryption":              enumFlag("md5", "scram-sha-256"),
	"huge_pages":                       enumFlag("off", "try", "on"),
	// WAL and replication.
	"max_wal_size":                 intFlag(2, maxInt, false),
	"min_wal_size":                 intFlag(32, maxInt, false),
	"wal_buffers":                  intFlag(-1, 262143, true),
	"wal_compression":              enumFlag("on", "off", "pglz", "lz4", "zstd"),
	"checkpoint_timeout":           intFlag(30, 86400, false),
	"checkpoint_completion_target": floatFlag(0, 1),
	"max_wal_senders":              intFlag(0, 262143, true),
	"max_replication_slots":        intFlag(0, 262143, true),
	"wal_sender_timeout":           intFlag(0, maxInt, false),
	"wal_receiver_timeout":         intFlag(0, maxInt, false),
	"max_standby_streaming_delay":  intFlag(-1, maxInt, false),
	"max_standby_archive_delay":    intFlag(-1, maxInt, false),
	"hot_standby_feedback":         boolFlag(false),
	"synchronous_commit":           enumFlag("local", "on", "off", "remote_apply", "remote_write"),
	"commit_delay":                 intFlag(0, 100000, false),
	"commit_siblings":              intFlag(0, 1000, false),
	"bgwriter_delay":               intFlag(10, 10000, false),
	"bgwriter_lru_maxpages":        intFlag(0, 1073741823, false),
	"bgwriter_lru_multiplier":      floatFlag(0, 10),
	// Autovacuum.
	"autovacuum":                      boolFlag(false),
	"autovacuum_max_workers":          intFlag(1, 262143, true),
	"autovacuum_naptime":              intFlag(1, 2147483, false),
	"autovacuum_vacuum_threshold":     intFlag(0, maxInt, false),
	"autovacuum_analyze_threshold":    intFlag(0, maxInt, false),
	"autovacuum_vacuum_scale_factor":  floatFlag(0, 100),
	"autovacuum_analyze_scale_factor": floatFlag(0, 100),
	"autovacuum_vacuum_cost_delay":    floatFlag(-1, 100),
	"autovacuum_vacuum_cost_limit":    intFlag(-1, 10000, false),
	"autovacuum_freeze_max_age":       intFlag(100000, 2000000000, true),
	"vacuum_cost_delay":               floatFlag(0, 100),
	"vacuum_cost_limit":               intFlag(1, 10000, false),
	// Query planning.
	"random_page_cost":               floatFlag(0, maxInt),
	"seq_page_cost":                  floatFlag(0, maxInt),
	"cpu_tuple_cost":                 floatFlag(0, maxInt),
	"cpu_index_tuple_cost":           floatFlag(0, maxInt),
	"cpu_operator_cost":              floatFlag(0, maxInt),
	"parallel_setup_cost":            floatFlag(0, maxInt),
	"parallel_tuple_cost":            floatFlag(0, maxInt),
	"default_statistics_target":      intFlag(1, 10000, false),
	"from_collapse_limit":            intFlag(1, maxInt, false),
	"join_collapse_limit":            intFlag(1, maxInt, false),
	"geqo_threshold":                 intFlag(2, maxInt, false),
	"jit":                            boolFlag(false),
	"jit_above_cost":                 floatFlag(-1, math.MaxFloat64),
	"plan_cache_mode":                enumFlag("auto", "force_custom_plan", "force_generic_plan"),
	"enable_bitmapscan":              boolFlag(false),
	"enable_hashagg":                 boolFlag(false),
	"enable_hashjoin":                boolFlag(false),
	"enable_indexonlyscan":           boolFlag(false),
	"enable_indexscan":               boolFlag(false),
	"enable_material":                boolFlag(false),
	"enable_mergejoin":               boolFlag(false),
	"enable_nestloop":                boolFlag(false),
	"enable_seqscan":                 boolFlag(false),
	"enable_sort":                    boolFlag(false),
	"enable_tidscan":                 boolFlag(false),
	"enable_partitionwise_join":      boolFlag(false),
	"enable_partitionwise_aggregate": boolFlag(false),
	// Statement behaviour.
	"statement_timeout":                   intFlag(0, maxInt, false),
	"lock_timeout":                        intFlag(0, maxInt, false),
	"idle_in_transaction_session_timeout": intFlag(0, maxInt, false),
	"idle_session_timeout":                intFlag(0, maxInt, false),
	"deadlock_timeout":                    intFlag(1, maxInt, false),
	"default_transaction_isolation":       enumFlag("serializable", "repeatable read", "read committed", "read uncommitted"),
	"default_transaction_read_only":       boolFlag(false),
	"session_replication_role":            enumFlag("origin", "replica", "local"),
	"search_path":                         strFlag(),
	"timezone":                            strFlag(),
	"datestyle":                           strFlag(),
	"intervalstyle":                       enumFlag("postgres", "postgres_verbose", "sql_standard", "iso_8601"),
	"client_min_messages":                 enumFlag("debug5", "debug4", "debug3", "debug2", "debug1", "log", "notice", "warning", "error"),
	// Logging.
	"log_checkpoints":             boolFlag(false),
	"log_connections":             boolFlag(false),
	"log_disconnections":          boolFlag(false),
	"log_duration":                boolFlag(false),
	"log_hostname":                boolFlag(false),
	"log_lock_waits":              boolFlag(false),
	"log_executor_stats":          boolFlag(false),
	"log_parser_stats":            boolFlag(false),
	"log_planner_stats":           boolFlag(false),
	"log_statement_stats":         boolFlag(false),
	"log_min_duration_statement":  intFlag(-1, maxInt, false),
	"log_autovacuum_min_duration": intFlag(-1, maxInt, false),
	"log_temp_files":              intFlag(-1, maxInt, false),
	"log_statement":               enumFlag("none", "ddl", "mod", "all"),
	"log_min_messages":            enumFlag(logLevels...),
	"log_min_error_statement":     enumFlag(logLevels...),
	"log_error_verbosity":         enumFlag("terse", "default", "verbose"),
	"log_timezone":                strFlag(),
	"track_activities":            boolFlag(false),
	"track_counts":                boolFlag(false),
	"track_io_timing":             boolFlag(false),
	"track_commit_timestamp":      boolFlag(true),
	"track_functions":             enumFlag("none", "pl", "all"),
	// Extensions (Recorded: the libraries are not preloaded).
	"pg_stat_statements.track":         recorded(enumFlag("none", "top", "all")),
	"pg_stat_statements.max":           recorded(intFlag(100, maxInt, true)),
	"pg_stat_statements.track_utility": recorded(boolFlag(false)),
	"pgaudit.log":                      recorded(flagDef{Type: "REPEATED_STRING", Values: []string{"read", "write", "function", "role", "ddl", "misc", "misc_set", "all", "none", "-read", "-write", "-function", "-role", "-ddl", "-misc", "-misc_set"}}),
	"pgaudit.log_relation":             recorded(boolFlag(false)),
	"auto_explain.log_min_duration":    recorded(intFlag(-1, maxInt, false)),
	"auto_explain.log_analyze":         recorded(boolFlag(false)),
	// Cloud SQL flags.
	"cloudsql.iam_authentication":                   recorded(boolFlag(false)),
	"cloudsql.logical_decoding":                     boolFlag(true),
	"cloudsql.enable_pgaudit":                       recorded(boolFlag(false)),
	"cloudsql.enable_pg_cron":                       recorded(boolFlag(true)),
	"cloudsql.enable_pglogical":                     recorded(boolFlag(true)),
	"cloudsql.enable_pg_squeeze":                    recorded(boolFlag(true)),
	"cloudsql.enable_pg_wait_sampling":              recorded(boolFlag(true)),
	"cloudsql.enable_auto_explain":                  recorded(boolFlag(true)),
	"cloudsql.enable_index_advisor":                 recorded(boolFlag(true)),
	"cloudsql.enable_anon":                          recorded(boolFlag(false)),
	"cloudsql.allow_passwordless_local_connections": recorded(boolFlag(false)),
	"cloudsql.pg_shadow_select_role":                recorded(strFlag()),
}

// validateFlags checks flags against the allow-list (FR-SQL-003) and
// returns them as a name → value map.
func validateFlags(flags []*sqladmin.DatabaseFlags, version string) (map[string]string, error) {
	major := pgVersions[version].Major
	out := map[string]string{}
	for _, f := range flags {
		if f == nil {
			continue
		}
		def, ok := flagDefs[f.Name]
		if !ok || (def.Since > 0 && major < def.Since) {
			return nil, errInvalid("Invalid flag name: %s.", f.Name)
		}
		if _, dup := out[f.Name]; dup {
			return nil, errInvalid("Duplicate flag: %s.", f.Name)
		}
		if err := def.validate(f.Name, f.Value); err != nil {
			return nil, err
		}
		out[f.Name] = f.Value
	}
	return out, nil
}

func (d flagDef) validate(name, v string) error {
	bad := func() error {
		return apierr.InvalidArgument("Invalid request: Invalid value for flag %s: %q.", name, v).WithLegacy("invalidFlagValue")
	}
	switch d.Type {
	case "BOOLEAN":
		if v != "on" && v != "off" {
			return bad()
		}
	case "INTEGER":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || float64(n) < d.Min || float64(n) > d.Max {
			return bad()
		}
	case "FLOAT":
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < d.Min || n > d.Max {
			return bad()
		}
	case "STRING":
		if len(d.Values) > 0 && !contains(d.Values, v) {
			return bad()
		}
	case "REPEATED_STRING":
		for _, p := range strings.Split(v, ",") {
			if len(d.Values) > 0 && !contains(d.Values, strings.TrimSpace(p)) {
				return bad()
			}
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// pgSettings maps validated flags to the PostgreSQL settings to apply.
func pgSettings(flags map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range flags {
		def := flagDefs[k]
		switch {
		case k == "cloudsql.logical_decoding":
			if v == "on" {
				out["wal_level"] = "logical"
			}
		case def.Recorded:
		default:
			out[k] = v
		}
	}
	return out
}

// needsRestart reports whether changing from old to new flags requires a
// server restart.
func needsRestart(old, new map[string]string) bool {
	for _, k := range unionKeys(old, new) {
		if old[k] != new[k] && flagDefs[k].Restart {
			return true
		}
	}
	return false
}

func unionKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// flagMap converts settings.databaseFlags to a map.
func flagMap(st *sqladmin.Settings) map[string]string {
	out := map[string]string{}
	if st == nil {
		return out
	}
	for _, f := range st.DatabaseFlags {
		if f != nil {
			out[f.Name] = f.Value
		}
	}
	return out
}

// listFlags implements flags.list (no permission is required in GCP
// beyond authentication; cloudsql.flags.list is not checked on a resource).
func (s *Service) listFlags(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("databaseVersion")
	versions := databaseVersions()
	if want != "" {
		if _, ok := pgVersions[want]; !ok {
			writeJSON(w, r, &sqladmin.FlagsListResponse{Kind: "sql#flagsList"})
			return
		}
		versions = []string{want}
	}
	resp := &sqladmin.FlagsListResponse{Kind: "sql#flagsList"}
	for _, name := range sortedKeys(flagDefs) {
		d := flagDefs[name]
		f := &sqladmin.Flag{Kind: "sql#flag", Name: name, Type: d.Type, RequiresRestart: d.Restart, AppliesTo: versions}
		switch d.Type {
		case "INTEGER", "FLOAT":
			f.MinValue = int64(d.Min)
			if d.Max < math.MaxInt64 {
				f.MaxValue = int64(d.Max)
			}
			f.ForceSendFields = []string{"MinValue"}
		case "STRING", "REPEATED_STRING":
			f.AllowedStringValues = d.Values
		}
		resp.Items = append(resp.Items, f)
	}
	writeJSON(w, r, resp)
}
