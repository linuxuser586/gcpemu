# The Cluster server's serving certificate is reissued on every start

A cluster's Public endpoint must be in the certificate it serves from the moment it is RUNNING (#644). When the External network moved to another range, the Cluster server sometimes served a certificate that named the cluster's previous external address for 1 to 3 seconds after `/readyz` answered, long enough for the cluster to go RUNNING (#886). The new address was in `--tls-san`, but k3s's dynamiclistener builds the new certificate from the one it stored in `server/tls/dynamic-cert.json` on the server's volume, keeps that certificate's resource version, and so treats the stored `k3s-serving` secret as unchanged until the secret is written again once the API server is ready. We decided that the Instance writes an empty `dynamic-cert.json` before every start of the Cluster server, which dynamiclistener treats as no certificate, so k3s issues one for the current `--tls-san` from its first handshake. And before a cluster goes RUNNING, the Instance completes a TLS handshake with the server under its Public endpoint's address and checks that the certificate names it, failing the start (ERROR) if it never does within the readiness wait.

## Considered options

- **Waiting in readiness alone.** It meets the rule, but every restart that changes an address waits out the race, and the cause stays.
- **Emptying the file only when `--tls-san` changed.** It needs a record of the previous start's addresses, for a saving of one serving key per start. The serving certificate carries nothing a client keeps: clients trust the cluster CA (`clusterCaCertificate`), `server-ca.crt`, which k3s loads separately and which never changes.
- **k3s's `server/tls/dynamic-cert-regenerate` marker.** Tried: the stale certificate was still served after `/readyz`. Regenerate reuses the stored names and goes through the same update queue.
- **Deleting the `k3s-serving` secret.** Not needed: once the secret is written, k3s merges the old and new names, so it never brings back a certificate without the current address.

## Consequences

- Every start of the Cluster server, the #840 restarts included, issues a new serving key under the same CA.
- The handshake is the guarantee, not the empty file: if a later k3s brings the race back, starts fail with an ERROR that names the Public endpoint and the names the certificate does cover, rather than going RUNNING with the wrong certificate.
- The same holds for every `--tls-san` address, `--gke-bind`'s included, though only the Public endpoint is checked.
