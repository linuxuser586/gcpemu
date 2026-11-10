# Incremental Base backups

Each Base backup of a Cloud SQL instance with point-in-time recovery is a
full, compressed `pg_basebackup` (`-Ft -z`). The emulator does not take
incremental Base backups that store only what changed since the previous
one.

## Why this is out of scope

- **Only some versions support it.** `pg_basebackup --incremental` was
  added in PostgreSQL 17. The emulator runs PostgreSQL 14 to 18, so 14,
  15 and 16 would need a second code path or would see no change.
- **More to set up, and restores get harder.** The server needs
  `summarize_wal = on`, which is a server setting the emulator would
  have to force on top of the user's database flags. Every point-in-time
  recovery would have to rebuild a full backup with `pg_combinebackup`
  from a chain of increments. Losing any increment would break every
  later Base backup in the chain, and transaction log retention would
  have to keep each chain whole instead of dropping Base backups one at
  a time.
- **Little left to save.** Base backups are already gzipped. On a 1.5 GB
  pgbench database, a compressed Base backup was 90 MB against 1.6 GB
  uncompressed. After #1038, an instance keeps about
  `transactionLogRetentionDays` + 1 of them. Most of the disk goes to the
  archived WAL, which an incremental Base backup does not shrink.

## Prior requests

- #1043: "Cloud SQL: backups take more disk than they need (uncompressed dumps, full Base backups)", option "make retained Base backups incremental"
