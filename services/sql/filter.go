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
