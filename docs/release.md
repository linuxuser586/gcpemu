# Release checklist

CI runs the VM relay path on Linux (the `vm-relay` job, `GCPEMU_VM_RELAY=1`), but hosted macOS
runners have no container runtime, and hosted Windows runners run only Windows containers.
Before tagging `vX.Y.Z`, run these checks by hand on macOS and on Windows.

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

## Windows (Containerized Instance)

Run every step below in PowerShell on **Docker Desktop** in Linux-containers mode. Podman machine
is best-effort: check it when one is at hand, with `GCPEMU_VM_RUNTIME=true`, but a release does
not wait for it.

Build the image from the commit to be tagged: `task release VERSION=vX.Y.Z` (it builds the Web
console first, so it needs Node and pnpm), then `docker build -t gcpemu:rc -f Dockerfile dist`.

### 1. Start the image with the socket and a port range

```powershell
docker run -d --name gcpemu -p 127.0.0.1:4510:4510 -p 127.0.0.1:20000-20019:20000-20019 `
  -v /var/run/docker.sock:/var/run/docker.sock `
  --group-add "$(docker run --rm -v /var/run/docker.sock:/s busybox:1.37 stat -c %g /s)" `
  gcpemu:rc start --ephemeral --port-range 20000-20019
docker exec gcpemu /gcpemu doctor
```

`doctor` must report the VM-runtime decision as on (Docker Desktop recognized) and the
loopback-alias decision as on. Point the clients on the host at the container:

```powershell
docker exec gcpemu /gcpemu env --shell powershell | Out-String | Invoke-Expression
Remove-Item Env:KUBECONFIG -ErrorAction SilentlyContinue   # a path inside the container
curl.exe "http://$env:GCPEMU_GATEWAY/"
```

`curl.exe` must get an answer from the API gateway: the addresses `env` prints must be ones a
Windows host can connect to. Repeat these three lines wherever a step below reloads the
environment.

### 2. A Cloud SQL instance from the host

```powershell
gcloud sql instances create one --database-version=POSTGRES_16 --tier=db-f1-micro
gcloud sql users set-password postgres --instance=one --password=pw
cloud-sql-proxy --port 15432 "$(gcloud sql instances describe one --format='value(connectionName)')"
```

In a second terminal, `psql "host=127.0.0.1 port=15432 user=postgres password=pw" -c 'select 1'`
must succeed. The instance's `PRIMARY` address must be a `127.0.0.x` of its own, as Docker
Desktop gets loopback aliases by default; while 5432 is free on the host, `psql` straight to that
address on 5432 must work too.

### 3. A GKE `LoadBalancer` Service from the host

```powershell
gcloud container clusters create lb --zone=us-central1-a --num-nodes=1
$p = gcloud config get-value project
curl.exe -o lb.kubeconfig "http://$env:GCPEMU_GATEWAY/container/_emu/kubeconfig?cluster=projects/$p/locations/us-central1-a/clusters/lb"
$env:KUBECONFIG = "$PWD\lb.kubeconfig"
kubectl create deployment web --image=nginx:1.29-alpine
kubectl expose deployment web --type=LoadBalancer --port=80
kubectl get service web           # EXTERNAL-IP is 127.0.0.1
docker exec gcpemu /gcpemu env --shell powershell | Out-String | Invoke-Expression
$env:KUBECONFIG = "$PWD\lb.kubeconfig"
curl.exe "http://$env:GCPEMU_GKE_LB_DEFAULT_WEB_80/"
```

`curl.exe` must get nginx's page on a port of the published range.

### 4. A forwarding rule from the host

```powershell
gcloud storage buckets create gs://lb-site
Set-Content -Path index.html -Value 'lb ok'
gcloud storage cp index.html gs://lb-site/index.html
gcloud compute backend-buckets create site --gcs-bucket-name=lb-site
gcloud compute url-maps create site --default-backend-bucket=site
gcloud compute target-http-proxies create site --url-map=site
gcloud compute forwarding-rules create site --global --target-http-proxy=site --ports=8080
docker exec gcpemu /gcpemu env --shell powershell | Out-String | Invoke-Expression
curl.exe "http://$env:GCPEMU_LB_SITE/index.html"
```

`GCPEMU_LB_SITE` must name `127.0.0.1` and a port of the published range, and `curl.exe` must
print `lb ok`.

Then `docker rm -f gcpemu`.
