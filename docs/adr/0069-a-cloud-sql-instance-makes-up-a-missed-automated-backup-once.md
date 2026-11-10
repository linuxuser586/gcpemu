# A Cloud SQL instance makes up a missed Automated backup once

Cloud SQL takes an Automated backup every day within a window that opens at `backupConfiguration.startTime`. The emulator took none, so a Cloud SQL instance with point-in-time recovery kept the Base backup from its creation forever, and transaction log retention, which anchors on a Base backup older than its cutoff, never dropped any of its WAL archive: 25 GB in a week on one machine (#1038). So a Cloud SQL instance that is running with backups enabled now takes an Automated backup as soon as the emulator clock passes its start time each day, with a Base backup when point-in-time recovery is on.

The emulator runs on machines that are often off, unlike Cloud SQL. Whether a day's slot has been handled is recorded on the Cloud SQL instance, not worked out from the backups that exist, because retention and users delete those. A slot is handled once its backup was attempted, whether or not the attempt succeeded. The first time the emulator finds a Cloud SQL instance running with an unhandled slot, whether the emulator was down or the Cloud SQL instance was stopped, it takes one Automated backup for the latest slot and marks every earlier one handled. Creating a Cloud SQL instance, enabling its backups or changing its start time marks the current slot handled, so the first backup comes at the next start time. A slot whose Cloud SQL instance is busy with another Operation stays unhandled until a later sweep finds it free, as Cloud SQL reschedules an Automated backup that meets an on-demand one. A failed attempt is recorded as a FAILED backup run and is not tried again before the next slot.

An Automated backup runs as a `BACKUP_VOLUME` Operation, like an on-demand one. Cloud SQL does not document whether its Automated backups appear in `operations.list`; the Operation is what keeps the backup from running alongside a restore or a settings change, and it shows a failure where users look for one.

Only successful Automated backups count toward `backupRetentionSettings.retainedBackups`, as Cloud SQL's documentation describes it. Retention applies after each Automated backup and whenever the count is lowered, and also deletes FAILED Automated backup runs older than the oldest one kept. A Base backup is still kept or dropped by transaction log retention alone, so deleting an Automated backup never shortens the Recovery window. With `retainBackupsOnDelete`, a deleted Cloud SQL instance's Automated backups become Retained backups that expire one a day, oldest first, the newest at the deletion time plus `retainedBackups` days, as Cloud SQL does.

## Considered options

- **One backup for each missed day.** Each would be a copy of the same data taken moments apart, and together they would push older, distinct backups out of the retained count.
- **Lose missed slots.** A laptop that is off at midnight every day would never take an Automated backup, which is the bug this fixes.
- **Work out handled slots from existing backups.** Deleting yesterday's Automated backup, by hand or through retention, would make its slot look missed and take another.
- **Try a failed backup again on every sweep.** A full disk, the commonest cause, would turn one failure into one attempt a second.
- **Start at a random time within Cloud SQL's 4-hour window.** It would be closer to Cloud SQL but harder to predict and to test, and it would make Deterministic Instances depend on the draw.
- **No Operation.** Backup expiry records none. An Automated backup, unlike expiry, runs on the Cloud SQL instance's database and must not overlap other Operations on it.

## Consequences

- A Cloud SQL instance with point-in-time recovery holds about `transactionLogRetentionDays` + 1 Base backups and that many days of WAL archive, and `retainedBackups` dumps, instead of growing without limit.
- A Cloud SQL instance can take two Automated backups less than a day apart: one made up when the emulator starts, then the next at its start time.
- In a Deterministic Instance, `gcpemu time advance` past the start time takes an Automated backup, but it does not move the Recovery window, which runs on the database clock (ADR 0022).
- Changing the start time to one earlier today does not take a backup today.
