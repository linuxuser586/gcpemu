package sql

import (
	"encoding/json"
	"regexp"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// Settings defaults and validation (FR-SQL-001). Tier, disk and
// availability type are Recorded: stored and reported, not enforced.

const (
	defaultTier     = "db-custom-1-3840"
	defaultPlusTier = "db-perf-optimized-N-2"
	// plusSince is the first major version on which an unset edition
	// means Enterprise Plus, as in GCP.
	plusSince = 16
)

var tierRe = regexp.MustCompile(`^db-[a-zA-Z0-9-]+$`)

// fromMap converts a JSON object into v.
func fromMap(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// toMap converts v into a JSON object.
func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// child returns the nested object at path, or nil.
func child(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		next, ok := m[p].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	return m
}

// has reports whether the nested key path is present.
func has(m map[string]any, path ...string) bool {
	parent := child(m, path[:len(path)-1]...)
	if parent == nil {
		return false
	}
	_, ok := parent[path[len(path)-1]]
	return ok
}

// merge applies a JSON merge patch: objects merge recursively, everything
// else (including arrays) replaces; null deletes.
func merge(dst, patch map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			dv, _ := dst[k].(map[string]any)
			dst[k] = merge(dv, pv)
		default:
			dst[k] = v
		}
	}
	return dst
}

// applySettingsDefaults fills settings the way Cloud SQL reports them.
// raw is the request's settings object (to tell absent from false).
func applySettingsDefaults(st *sqladmin.Settings, raw map[string]any, zone, version string, create bool) {
	st.Kind = "sql#settings"
	if st.Edition == "" || st.Edition == "EDITION_UNSPECIFIED" {
		st.Edition = "ENTERPRISE"
		if strings.HasPrefix(st.Tier, "db-perf-optimized-") || pgVersions[version].Major >= plusSince {
			st.Edition = "ENTERPRISE_PLUS"
		}
	}
	if st.Tier == "" {
		st.Tier = defaultTier
		if st.Edition == "ENTERPRISE_PLUS" {
			st.Tier = defaultPlusTier
		}
	}
	if st.ActivationPolicy == "" || st.ActivationPolicy == "SQL_ACTIVATION_POLICY_UNSPECIFIED" {
		st.ActivationPolicy = "ALWAYS"
	}
	if st.AvailabilityType == "" || st.AvailabilityType == "SQL_AVAILABILITY_TYPE_UNSPECIFIED" {
		st.AvailabilityType = "ZONAL"
	}
	if st.DataDiskSizeGb == 0 {
		st.DataDiskSizeGb = 10
	}
	if st.DataDiskType == "" || st.DataDiskType == "SQL_DATA_DISK_TYPE_UNSPECIFIED" {
		st.DataDiskType = "PD_SSD"
	}
	if st.PricingPlan == "" || st.PricingPlan == "SQL_PRICING_PLAN_UNSPECIFIED" {
		st.PricingPlan = "PER_USE"
	}
	if st.ReplicationType == "" {
		st.ReplicationType = "SYNCHRONOUS"
	}
	if st.ConnectorEnforcement == "" || st.ConnectorEnforcement == "CONNECTOR_ENFORCEMENT_UNSPECIFIED" {
		st.ConnectorEnforcement = "NOT_REQUIRED"
	}
	if create && !has(raw, "storageAutoResize") {
		t := true
		st.StorageAutoResize = &t
	}
	if st.BackupConfiguration == nil {
		st.BackupConfiguration = &sqladmin.BackupConfiguration{}
	}
	bc := st.BackupConfiguration
	bc.Kind = "sql#backupConfiguration"
	if bc.StartTime == "" {
		bc.StartTime = "00:00"
	}
	if bc.BackupRetentionSettings == nil {
		bc.BackupRetentionSettings = &sqladmin.BackupRetentionSettings{RetentionUnit: "COUNT", RetainedBackups: 7}
	}
	if bc.TransactionLogRetentionDays == 0 {
		bc.TransactionLogRetentionDays = 7
	}
	if bc.TransactionalLogStorageState == "" && bc.PointInTimeRecoveryEnabled {
		bc.TransactionalLogStorageState = "CLOUD_STORAGE"
	}
	if st.IpConfiguration == nil {
		st.IpConfiguration = &sqladmin.IpConfiguration{}
	}
	ipc := st.IpConfiguration
	if create && !has(raw, "ipConfiguration", "ipv4Enabled") {
		ipc.Ipv4Enabled = true
	}
	switch {
	case ipc.SslMode == "" || ipc.SslMode == "SSL_MODE_UNSPECIFIED":
		ipc.SslMode = sslMode(&sqladmin.IpConfiguration{RequireSsl: ipc.RequireSsl})
	default:
		ipc.RequireSsl = ipc.SslMode == "TRUSTED_CLIENT_CERTIFICATE_REQUIRED"
	}
	if ipc.ServerCaMode == "" || ipc.ServerCaMode == "CA_MODE_UNSPECIFIED" {
		ipc.ServerCaMode = "GOOGLE_MANAGED_INTERNAL_CA"
	}
	for _, a := range ipc.AuthorizedNetworks {
		if a != nil {
			a.Kind = "sql#aclEntry"
		}
	}
	if st.LocationPreference == nil {
		st.LocationPreference = &sqladmin.LocationPreference{}
	}
	st.LocationPreference.Kind = "sql#locationPreference"
	if st.LocationPreference.Zone == "" {
		st.LocationPreference.Zone = zone
	}
	if st.MaintenanceWindow != nil {
		st.MaintenanceWindow.Kind = "sql#maintenanceWindow"
	}
	if st.SettingsVersion == 0 {
		st.SettingsVersion = 1
	}
}

// validateSettings checks fields Cloud SQL validates synchronously.
func validateSettings(st *sqladmin.Settings, version string) (map[string]string, error) {
	if !tierRe.MatchString(st.Tier) {
		return nil, errInvalid("Invalid Tier (%s) for (%s) Edition.", st.Tier, st.Edition)
	}
	perf := strings.HasPrefix(st.Tier, "db-perf-optimized-")
	switch st.Edition {
	case "ENTERPRISE":
		if perf {
			return nil, errInvalid("Invalid Tier (%s) for (%s) Edition.", st.Tier, st.Edition)
		}
	case "ENTERPRISE_PLUS":
		if !perf {
			return nil, errInvalid("Invalid Tier (%s) for (%s) Edition. Set settings.edition to ENTERPRISE for this tier; POSTGRES_%d and later default to ENTERPRISE_PLUS.", st.Tier, st.Edition, plusSince)
		}
	default:
		return nil, errInvalid("Invalid edition %s.", st.Edition)
	}
	switch st.ActivationPolicy {
	case "ALWAYS", "NEVER":
	default:
		return nil, errInvalid("Invalid activation policy %s for a PostgreSQL instance.", st.ActivationPolicy)
	}
	switch st.AvailabilityType {
	case "ZONAL", "REGIONAL":
	default:
		return nil, errInvalid("Invalid availability type %s.", st.AvailabilityType)
	}
	ipc := st.IpConfiguration
	if !ipc.Ipv4Enabled && ipc.PrivateNetwork == "" && (ipc.PscConfig == nil || !ipc.PscConfig.PscEnabled) {
		return nil, errInvalid("At least one of public IP or private IP must be enabled.")
	}
	switch ipc.SslMode {
	case "ALLOW_UNENCRYPTED_AND_ENCRYPTED", "ENCRYPTED_ONLY", "TRUSTED_CLIENT_CERTIFICATE_REQUIRED":
	default:
		return nil, errInvalid("Invalid SSL mode %s.", ipc.SslMode)
	}
	for _, a := range ipc.AuthorizedNetworks {
		if a == nil {
			continue
		}
		if !strings.Contains(a.Value, "/") {
			if !validIPv4(a.Value) {
				return nil, errInvalid("Non-routable or private authorized network (%s).", a.Value)
			}
		} else if !validCIDR(a.Value) {
			return nil, errInvalid("Invalid authorized network (%s).", a.Value)
		}
	}
	return validateFlags(st.DatabaseFlags, version)
}

func validIPv4(s string) bool { _, ok := parseIPv4(s); return ok }

func validCIDR(s string) bool {
	ip, bits, ok := strings.Cut(s, "/")
	if !ok || !validIPv4(ip) {
		return false
	}
	n := 0
	for _, c := range bits {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return bits != "" && n <= 32
}

func parseIPv4(s string) ([4]byte, bool) {
	var out [4]byte
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return out, false
	}
	for i, p := range parts {
		if p == "" || len(p) > 3 {
			return out, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return out, false
		}
		out[i] = byte(n)
	}
	return out, true
}

// serviceAccountFor returns the instance's service account email, as GCP
// assigns per project: p<number>-<suffix>@gcp-sa-cloud-sql.iam.gserviceaccount.com.
func serviceAccountFor(proj, suffix string) string {
	return "p" + project.NumberString(proj) + "-" + suffix + "@gcp-sa-cloud-sql.iam.gserviceaccount.com"
}
