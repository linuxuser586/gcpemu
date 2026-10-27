# Firestore's and Datastore's Managed emulators keep their data

Amends ADR 0029.

ADR 0029 keeps every Managed emulator's data in memory only, so a Persistent Instance loses Firestore and Datastore documents on every stop, and State snapshots leave them out. We decided that Firestore's and Datastore's Managed emulators keep their data across a clean stop of a Persistent Instance, and that State snapshots hold it. Spanner's and Bigtable's stay in memory only, and a Persistent Instance that manages either logs one line at start naming them.

**Exported through the emulator's HTTP API, not its flags.** The emulator cannot list the databases it holds, so the API gateway records, in the Instance's Store, each project and database it forwards to one of the two; Datastore's default database counts as one. On a clean stop of a Persistent Instance, while the container still runs, the Instance exports each recorded database with `POST /emulator/v1/projects/P:export`, copies the exports out as that API's saved data in the data directory, replacing what was there, and then removes the container as before. The whole step is time-bounded. When it starts, the Instance copies saved data into the new container and imports each database with `POST /emulator/v1/projects/P:import`; it is not ready until every import is done.

**A failed export keeps the data saved before.** If the container is gone or does not answer, the Instance logs an error naming the API, saying that data written since the emulator started is lost, and carries on stopping. There is no periodic export, so a crash loses everything since the last clean stop.

**Reset and State snapshots.** Reset deletes every API's saved data and the record of databases, and replaces the containers. Saving a State snapshot exports each running Firestore or Datastore Managed emulator into the snapshot; the record of databases is in the Store, which the snapshot holds. Restoring one makes the snapshot's data the saved data and imports it into the container Reset replaced, as import merges into existing data rather than replacing it. Restoring into an Instance that does not manage the API only stores it as saved data. Saved data of an API the Instance does not manage now is left alone, and imported the next time it is managed.

## Considered options

- **`--export-on-exit` and `--import-data`.** Export on exit does not run on SIGTERM, which is how a container is stopped; where it does run, it fails on named Firestore databases and writes every Datastore database into one directory. `--import-data` takes a single export and shows it in every project.
- **Periodic export**, to survive a crash or a Disrupted container. Each export copies every database, and a crash recovers only up to the last one anyway; left for a later decision.
- **Keeping the container and its data between runs**, by stopping rather than removing it. It would be the only Managed container kept across a stop, and Reset and State snapshots would still need export and import.

## Consequences

- Stopping a Persistent Instance takes longer by the time the exports take, up to the bound.
- A database that a client reaches without going through the API gateway is not recorded, and so not kept. Clients reach a Managed emulator only through the gateway.
- Spanner and Bigtable still lose their data on every stop.
