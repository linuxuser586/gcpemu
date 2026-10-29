# Read-through for pods

A GKE pod does not get a Read-through. A pod that reads a public bucket
on real Cloud Storage, such as `chrome-for-testing-public` behind
Playwright's browser downloads or `proxy-golang-org-prod` behind the Go
module proxy, gets 404 `NoSuchBucket` from the Instance, because the
node's DNS sends `storage.googleapis.com` to the Google front end. No
list of buckets in `gcpemu.yaml` changes that.

The answer for such a pod is the label from ADR 0019:

```yaml
metadata:
  labels:
    gcpemu.dev/googleapis: real
```

The pod then resolves `*.googleapis.com` to real Google, as on GCP, and
every other name through the cluster.

## Why this is out of scope

ADR 0019 considered reading buckets the Instance doesn't have through
from real GCS for pods, and chose the label instead. The emulator never
calls real Google on a client's behalf (NFR-SEC-002), and a pod already
has a per-workload way to reach Google itself. ADR 0021 made the
Read-through exception only for `--gfe-bind`, because a machine on the
LAN has no such choice the Instance could offer. A pod does.

An allowlist would narrow the exception to named buckets, but it would
still make the Instance call Google for clients that can do it
themselves, and it would put a list of real buckets into config that
every Instance in a team has to keep in step.

The label's cost is that it is all or nothing: every `googleapis.com`
call from a labelled pod goes to real Google, so the pod loses the
emulated Services and the Instance's Workload Identity tokens. That
only matters to a pod that needs both public buckets and emulated
Services. None has been reported yet; the pods that hit this (#427,
#845) only download.

A proposal to give pods a Read-through should name a pod that needs
both, say why it can't be split into one that downloads and one that
calls the emulated Services, and come with an ADR amending ADR 0019.

## Prior requests

- #845: "Pods can't read public Cloud Storage buckets: storage.googleapis.com is gcpemu in-cluster"
