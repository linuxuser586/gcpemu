# Release checklist

CI runs the VM relay path on Linux (the `vm-relay` job, `GCPEMU_VM_RELAY=1`), but hosted macOS
runners have no container runtime. Before tagging `vX.Y.Z`, run these checks by hand on macOS.

## Release notes

The release lists the merged pull requests on its own. For a release that users need to act on,
commit `docs/releases/vX.Y.Z.md` before tagging: the workflow puts it above that list.

## macOS (Apple Silicon)

Run every step below on **Docker Desktop** and on **Colima**. OrbStack and Podman machine are
best-effort: check them when one is at hand, but a release does not wait for them.

Switch runtimes with `docker context use <name>` (or `DOCKER_HOST`), and confirm the choice with
`bin/gcpemu doctor`: it names the runtime it picked and reports the VM relay as on.

Start from a clean checkout of the commit to be tagged, and run `task build`.

### 1. Reference stack

```sh
task e2e
```

It must pass within its 10-minute budget. See "Reference stack (SRS 11.2)" in the README.

### 2. Two Cloud SQL instances at once, over loopback aliases

```sh
sudo bin/gcpemu loopback install
bin/gcpemu doctor                 # reports the 127.0.0.x aliases as present
bin/gcpemu start --detach --services sql
eval "$(bin/gcpemu env)"
gcloud sql instances create one --database-version=POSTGRES_16 --tier=db-f1-micro
gcloud sql instances create two --database-version=POSTGRES_16 --tier=db-f1-micro
gcloud sql users set-password postgres --instance=one --password=pw
gcloud sql users set-password postgres --instance=two --password=pw
eval "$(bin/gcpemu env)"          # now with GCPEMU_SQL_ONE and GCPEMU_SQL_TWO
```

`GCPEMU_SQL_ONE` and `GCPEMU_SQL_TWO` must be on different `127.0.0.x` addresses. Connect to both
at the same time, for example with `psql` in two terminals, and run `select 1` in each. Then
`bin/gcpemu stop` and `sudo bin/gcpemu loopback uninstall`.

### 3. A GKE `LoadBalancer` Service from the host

```sh
bin/gcpemu start --detach --services gke
eval "$(bin/gcpemu env)"
gcloud container clusters create lb --zone=us-central1-a --num-nodes=1
gcloud container clusters get-credentials lb --zone=us-central1-a
kubectl create deployment web --image=nginx:1.29-alpine
kubectl expose deployment web --type=LoadBalancer --port=80
kubectl get service web           # EXTERNAL-IP is a loopback address
eval "$(bin/gcpemu env)"          # GCPEMU_GKE_LB_DEFAULT_WEB_80
curl "http://$GCPEMU_GKE_LB_DEFAULT_WEB_80/"
```

`curl` must get nginx's page at the address the Service reports. Then `bin/gcpemu stop`.

### 4. VLAN (8021q) support

Fill in the runtime table under "Container runtime" in the README (it starts as "not yet
checked") for each runtime you ran. The reference stack doesn't use a Translated network, so check
it directly:

```sh
go test -count=1 -v -run 'TestEgressThroughNATTranslated|TestTranslatedNetworkSurvivesRestart' ./services/compute
```

The tests make a runtime network that overlaps the subnetwork's range, so the subnetwork is
Translated. They pass on a runtime with VLAN support. On one without it they fail with an error
that names the runtime and says it has no VLAN (8021q) support, not a raw `ip` error. A skip means
the check didn't run. Record "supported" or "not supported", with the runtime's version.

Commit the table change before tagging.
