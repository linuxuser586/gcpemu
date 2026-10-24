# LAN bindings for the Kubernetes API and the registry accept only real credentials

A shared Instance on an office machine is managed from another cluster on the LAN: Argo CD deploys into an emulated GKE cluster and BuildKit pushes to the emulated Artifact Registry. `--bind` would expose the Admin API and every Data plane, so `--gke-bind` and `--ar-bind` expose just those two, like `--lb-bind` (#120), and need no `--i-understand-this-is-insecure`. That is safe only if nothing reaches either one without credentials the Instance issued, whatever the IAM enforcement level, since audit (the default) and off let anonymous callers through on loopback.

**Kubernetes API.** The emulator relays TCP from each `--gke-bind` address to the API server, and `--tls-san` adds the address to the server's certificate. TLS ends at the API server, so client certificates and service account tokens work unchanged. k3s refuses anonymous requests. The remaining gap was the TokenReview webhook, which outside enforce mode maps any unknown bearer token to the Default principal. With `--gke-bind` it no longer does, for every cluster and on loopback too, because the webhook cannot tell which listener a request came through.

**Registry.** The `--ar-bind` listeners terminate TLS with a certificate from the instance CA, require credentials that authenticate on every request except the token endpoint, and never hand out the anonymous token. IAM permissions are then checked at the enforcement level as usual. They challenge for Basic credentials, not a Bearer realm, because go-containerregistry (crane, ko) refuses a token realm on a private IP literal as an SSRF guard.

## Considered options

- **Terminating TLS for the Kubernetes API in the emulator**, like the Connect gateway (ADR 0002). The emulator could then refuse unknown tokens only on the LAN path. But it would have to verify client certificates itself and forward with impersonation, and service account tokens would need a TokenReview round trip. The relay is far simpler, and the stricter webhook costs little: a garbage token on loopback is refused instead of being treated as the Default principal.
- **Requiring enforce mode for these flags.** This would close the same gaps, but it would also force IAM policy on everything else for a deployment that only wants these two endpoints protected.
- **A Bearer challenge on the registry, as on loopback.** It would be closer to GCP, but crane, ko and other go-containerregistry clients could not log in by IP address.

## Consequences

- Real credentials still work: emulator IAM access tokens, service account keys (`_json_key`) and, for Kubernetes, client certificates and service account tokens.
- An unknown bearer token is refused by every cluster's API server while `--gke-bind` is set, including on loopback.
- The registry port on a LAN address is the loopback registry's port when it is free there, so `docker login <address>:5000` matches what users see locally.
- `--sql-bind` (#620) follows the same rule for Cloud SQL. Postgres needs a password for built-in users and an Instance-issued token for IAM users, at every IAM enforcement level, so the emulator relays TCP and adds no check of its own. How connectors reach every instance on one address is ADR 0028, and how authorized networks see a LAN caller's own address is ADR 0030.
- A Containerized Instance on a bridge network can serve these flags only on its container's own addresses, which only container networks reach, never the LAN (#497). Publishing a port on a host address is no substitute: the published listeners are `--bind`'s, which don't require credentials, and the certificates name the container's address, not the host's. Supporting it would need a second way to name the host's address and map each listener to a published port, so we chose not to. To reach the LAN, run the image with `--network host` on Linux, where the Instance runs as on the host, or the native binary on a VM runtime, whose host network is the VM's. The same holds for `--lb-bind`, `--gfe-bind` (ADR 0021) and `--sql-bind`.
