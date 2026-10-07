package sql

import "sort"

// pgVersion describes one supported databaseVersion (FR-SQL-001): the
// container image (pinned by tag; digest pinning is a release task), the
// installed minor version reported as databaseInstalledVersion, and where
// the image keeps its data directory.
type pgVersion struct {
	Image     string
	Installed string // e.g. "POSTGRES_17_10"
	Major     int
	// DataMount is the volume mount point (PGDATA or its parent).
	DataMount string
}

// pgVersions maps databaseVersion to the image that runs it.
var pgVersions = map[string]pgVersion{
	"POSTGRES_14": {Image: "postgres:14.23-alpine", Installed: "POSTGRES_14_23", Major: 14, DataMount: "/var/lib/postgresql/data"},
	"POSTGRES_15": {Image: "postgres:15.18-alpine", Installed: "POSTGRES_15_18", Major: 15, DataMount: "/var/lib/postgresql/data"},
	"POSTGRES_16": {Image: "postgres:16.14-alpine", Installed: "POSTGRES_16_14", Major: 16, DataMount: "/var/lib/postgresql/data"},
	"POSTGRES_17": {Image: "postgres:17.10-alpine", Installed: "POSTGRES_17_10", Major: 17, DataMount: "/var/lib/postgresql/data"},
}

// defaultDatabaseVersion is used when insert omits databaseVersion. GCP
// requires the field for PostgreSQL; gcloud defaults to the newest version.
const defaultDatabaseVersion = "POSTGRES_17"

// databaseVersions returns the supported versions in order.
func databaseVersions() []string {
	out := make([]string, 0, len(pgVersions))
	for k := range pgVersions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
