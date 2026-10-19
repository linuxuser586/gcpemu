package sql

import (
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/listfilter"
)

// List filters, as gcloud's sql filter rewrite sends them to the server
// (internal/listfilter).

// backupFilterFields are the backups.list filter fields.
var backupFilterFields = map[string]listfilter.Field[*sqladmin.Backup]{
	"instance": {Get: func(b *sqladmin.Backup) string { return b.Instance }},
	"type":     {Get: func(b *sqladmin.Backup) string { return b.Type }},
	"location": {Get: func(b *sqladmin.Backup) string { return b.Location }},
	"backupInterval.startTime": {Time: true, Get: func(b *sqladmin.Backup) string {
		if b.BackupInterval == nil {
			return ""
		}
		return b.BackupInterval.StartTime
	}},
	"instanceDeletionTime": {Time: true, Get: func(b *sqladmin.Backup) string { return b.InstanceDeletionTime }},
}

// instanceFilterFields are the instances.list filter fields. The API
// takes any JSON field path; the emulator takes only these: the ones
// Terraform's google_sql_database_instances sends, instanceType and
// labels. : is an exact match on every one of them.
var instanceFilterFields = map[string]listfilter.Field[*sqladmin.DatabaseInstance]{
	"name":            {Exact: true, Get: func(in *sqladmin.DatabaseInstance) string { return in.Name }},
	"state":           {Exact: true, Get: func(in *sqladmin.DatabaseInstance) string { return in.State }},
	"region":          {Exact: true, Get: func(in *sqladmin.DatabaseInstance) string { return in.Region }},
	"databaseVersion": {Exact: true, Get: func(in *sqladmin.DatabaseInstance) string { return in.DatabaseVersion }},
	"gceZone":         {Exact: true, Get: func(in *sqladmin.DatabaseInstance) string { return in.GceZone }},
	"instanceType":    {Exact: true, Get: func(in *sqladmin.DatabaseInstance) string { return in.InstanceType }},
	"settings.tier": {Exact: true, Get: func(in *sqladmin.DatabaseInstance) string {
		if in.Settings == nil {
			return ""
		}
		return in.Settings.Tier
	}},
	"settings.userLabels.": {Exact: true, Key: func(in *sqladmin.DatabaseInstance, key string) string {
		if in.Settings == nil {
			return ""
		}
		return in.Settings.UserLabels[key]
	}},
}
