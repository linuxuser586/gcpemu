# The WAL archive stores segments gzipped and the agent decompresses them on restore

A Cloud SQL instance with point-in-time recovery archived each completed WAL segment uncompressed, and its archived WAL was often the largest part of its WAL archive: about 24 GB for 3.4 GB of WAL a day kept for 7 days (#1038). WAL compresses well: `gzip -1` took pgbench's WAL from 1,325 MB to 83 MB and the WAL of loading random data from 336 MB to 162 MB. So PostgreSQL's `archive_command` now stores each segment as `<segment>.gz`, compressed with busybox `gzip -1` (#1048).

The command writes a hidden temporary file in the WAL archive and renames it into place, so a crash of PostgreSQL or the container never leaves a partial segment. It never overwrites: when the segment is already stored, gzipped or uncompressed, it succeeds if the stored content is identical and fails if it differs, as PostgreSQL's continuous-archiving documentation asks. Without that, an archiver that crashed after storing a segment but before recording success failed every retry, and `pg_wal` grew without limit.

When a clone's container unpacks a restore bundle, the agent decompresses every gzipped file in it to its own name, so `restore_command` stays a plain `cp`. A WAL archive written before this change keeps its uncompressed segments next to new gzipped ones; nothing is migrated, and transaction log retention ages the old ones out. Listing, retention, the Recovery window and restore bundles accept both forms.

Because a Cloud SQL instance's record shows only that point-in-time recovery is on, not the command, every start of its container compares the server's `archive_command` with the emulator's and updates it with `ALTER SYSTEM` and a reload when they differ. A failed update is logged and does not fail the start, because an older command still archives. A clone that starts from a restore bundle skips this, because finishing its recovery applies its settings anyway.

## Considered options

- **`archive_library`.** PostgreSQL 14, which the emulator supports, has none.
- **zstd or lz4.** They are faster or smaller, but `gzip` is the one compressor in every `postgres:*-alpine` image the emulator uses, and the agent can decompress it with Go's standard library.
- **Decompress in `restore_command`.** Each fetch would need a shell test for which form exists. Decompressing once in the agent keeps the command PostgreSQL runs during recovery trivial and testable without a container.
- **Recompress the existing WAL archive.** It would rewrite gigabytes on upgrade to save space that retention frees within `transactionLogRetentionDays` anyway.
- **Overwrite a stored segment that differs.** It would hide a real divergence. PostgreSQL's documentation asks the command to fail.
- **Sync the temporary file before renaming it.** busybox cannot sync a single file in every image, and syncing everything on each segment is costly on a VM runtime. A crash of the emulator's host already puts the instance's data volume at risk.
- **Force `wal_compression`.** It is a database flag users set.

## Consequences

- A WAL archive is several times smaller for most workloads; random data compresses about 2×.
- After a crash of the emulator's host, an empty or truncated gzipped segment fails every retry, and archiving stalls until the file is deleted.
- A future change of compressor has to handle WAL archives holding gzipped and uncompressed segments, and possibly a third form, until retention ages them out.
