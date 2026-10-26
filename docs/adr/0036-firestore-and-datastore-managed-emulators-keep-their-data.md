# Firestore's and Datastore's Managed emulators keep their data

Amends ADR 0029.

ADR 0029 keeps every Managed emulator in memory only, so a Persistent Instance loses their data on every stop and State snapshots do not hold it. We decided that a Persistent Instance keeps the data of its Firestore and Datastore Managed emulators across a clean stop and start, and that State snapshots hold it. Spanner's and Bigtable's emulators cannot export their data, so they stay in memory only. A Persistent Instance logs which of its Managed emulators keep no data when it starts.

**Exported through the emulator's API.** While the emulator still runs, the Instance exports each Firestore or Datastore database it knows of through the emulator's own export endpoint and keeps the exports in its data directory. It then removes the container as it does any Managed container. On the next start it imports each export before the Instance is ready. The emulator cannot list the databases it holds, so the Instance records each project and database that the API gateway forwards to a Managed emulator.

**Lost since the last start on a crash.** There is no periodic export. A Managed emulator that is killed, Disrupted or not reachable when the Instance stops loses what was written since it started. The Instance keeps the data it saved before and logs that the newer data is lost.

**Reset and State snapshots.** Reset deletes the saved data and the record of databases. Saving a State snapshot exports each database into the snapshot. Restoring one replaces the emulator's container before importing, as an import merges into the data already there. An Instance that does not run that API's Managed emulator keeps the saved data untouched until it does, or until Reset.

## Considered options

- **The emulator's `--export-on-exit` and `--import-data` flags.** The Instance force-removes its containers, and the emulator does not export on SIGTERM either, through `gcloud` or sent to Java directly. Where export on exit does run, it fails on a named database in Firestore mode and writes every database to the same directory in Datastore mode, so only one survives. `--import-data` takes a single export and shows it in every project.
- **Periodic export** while the emulator runs. It would bound the loss on a crash, at the cost of load and exports racing with writes, for a local emulator whose user already has to stop and start the Instance after a Disrupted container.
- **Keeping all four in memory**, as ADR 0029 does for consistency. Firestore and Datastore are the emulators people seed and work against across sessions, and losing their data is a limitation rather than behavior anyone depends on.
- **Exporting only each known Project's default database.** It would miss named databases and Projects that exist only in the emulator.

## Consequences

- ADR 0029's "In memory only" no longer holds for Firestore and Datastore. A Managed emulator, a Persistent Instance and a State snapshot differ by API in what they keep.
- Stopping an Instance waits, within a bound, for the exports, and starting one is not ready until the imports are done.
