package sql

import "sort"

// pgVersion describes one supported databaseVersion (FR-SQL-001): the
// container image (pinned by tag; digest pinning is a release task), the
// installed minor version reported as databaseInstalledVersion, and where
// the image keeps its data directory.
type pgVersion struct {
	Image     string
	Installed string // e.g. "POSTGRES_17_11"
	Major     int
	// DataMount is the volume mount point (PGDATA or its parent).
	DataMount string
}

// pgVersions maps databaseVersion to the image that runs it.
var pgVersions = map[string]pgVersion{
	"POSTGRES_14": {Image: "postgres:14.24-alpine", Installed: "POSTGRES_14_24", Major: 14, DataMount: "/var/lib/postgresql/data"},
	"POSTGRES_15": {Image: "postgres:15.19-alpine", Installed: "POSTGRES_15_19", Major: 15, DataMount: "/var/lib/postgresql/data"},
	"POSTGRES_16": {Image: "postgres:16.15-alpine", Installed: "POSTGRES_16_15", Major: 16, DataMount: "/var/lib/postgresql/data"},
	"POSTGRES_17": {Image: "postgres:17.11-alpine", Installed: "POSTGRES_17_11", Major: 17, DataMount: "/var/lib/postgresql/data"},
	// From 18 the image keeps PGDATA in /var/lib/postgresql/18/docker.
	"POSTGRES_18": {Image: "postgres:18.6-alpine", Installed: "POSTGRES_18_6", Major: 18, DataMount: "/var/lib/postgresql"},
}

// maintenanceSuffix is the Cloud SQL maintenance release the version table
// matches. An instance's maintenanceVersion is its Installed version
// followed by this suffix.
const maintenanceSuffix = ".R20261013.02_04"

// maintenanceVersion returns the maintenanceVersion of an instance running
// v.
func (v pgVersion) maintenanceVersion() string {
	return v.Installed + maintenanceSuffix
}

// defaultDatabaseVersion is used when insert omits databaseVersion. GCP
// requires the field for PostgreSQL; gcloud defaults to the newest version.
const defaultDatabaseVersion = "POSTGRES_18"

// databaseVersions returns the supported versions in order.
func databaseVersions() []string {
	out := make([]string, 0, len(pgVersions))
	for k := range pgVersions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProductImages lists the postgres image of every supported
// databaseVersion, for the weekly image report (#1165).
func ProductImages() []string {
	out := make([]string, 0, len(pgVersions))
	for _, v := range databaseVersions() {
		out = append(out, pgVersions[v].Image)
	}
	return out
}
