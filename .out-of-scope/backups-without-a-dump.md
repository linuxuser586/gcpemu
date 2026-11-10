# Backups without a dump when point-in-time recovery is on

Every on-demand backup, Automated backup and Final backup of a Cloud SQL
instance stores a `pg_dumpall` of the whole instance, even when
point-in-time recovery is on and the instance's Base backups and WAL
archive could rebuild the same data. The emulator does not skip the dump
and restore from the archive instead.

## Why this is out of scope

A backup and the Base backup taken with it live for different lengths of
time, and only the dump lets the backup outlive its Base backup:

- **Retention is set separately.** `retainedBackups` decides how many
  Automated backups are kept. Transaction log retention alone decides how
  long a Base backup is kept (`transactionLogRetentionDays`). With
  `retainedBackups` = 30 and 7 days of log retention, the 8th-oldest
  backup has no Base backup left to restore from.
- **Some backups outlive the instance.** A Final backup, and a Retained
  backup after `retainBackupsOnDelete`, belong to an instance that no
  longer exists. Its WAL archive is on a volume deleted with it.
- **Restore and clone target other instances.** Restoring a backup into
  another instance would have to copy the source's Base backup and WAL
  across instances, and the source may be stopped or busy.

Making Base backups last as long as their backups would mean deleting a
backup could shorten the Recovery window. That breaks the rule that
transaction log retention alone keeps or drops a Base backup (ADR 0069).

Disk use from dumps is handled by compressing them (#1043), not by
dropping them.

## Prior requests

- #1043: "Cloud SQL: backups take more disk than they need (uncompressed dumps, full Base backups)", option "skip the dump when PITR is on"
