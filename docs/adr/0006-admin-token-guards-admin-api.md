# Beyond loopback, an Admin token guards the Admin API and the Web console, not the public APIs

FR-UI-009 asked for a console token when `--bind` leaves loopback, but the console only calls the public APIs and the Admin API, so guarding its HTML alone would protect nothing: anyone who can reach the API gateway could still call `/_emu/v1/reset`, `shutdown` or `seed`. We decided that an Admin token guards every `/_emu/*` route and `/console` on a non-loopback bind, while the public GCP APIs stay under IAM, which already cannot be `off` beyond loopback without `--i-understand-this-is-insecure`. The token is random per start unless `GCPEMU_ADMIN_TOKEN` sets it. It is written 0600 to the instance dir, and it travels in an `X-Gcpemu-Admin-Token` header or in an `HttpOnly; SameSite=Strict` cookie that the browser gets from the URL printed at startup. `GET /_emu/v1/health` and `/ready` are exempt so that probes work.

## Considered options

- **Guarding only the console assets.** This was rejected as theater, for the reason above.
- **Guarding the public APIs too.** This would compete with IAM, and SDKs and gcloud have no way to send an extra token.
- **Carrying the token in `Authorization: Bearer`.** That header already identifies the Principal on service-registered `/_emu/v1` routes and on public APIs (and the Principal switcher sends it), so one header cannot carry both.
- **Exempting requests from loopback peers on a wide bind.** Container traffic arrives through the in-process netplane forwarder and would look local, so this exemption would defeat the guard.

## Consequences

- On loopback nothing changes, so existing scripts, tests and the setup-gcpemu Action keep working.
- The GKE `/container/_emu/*` hooks keep their per-cluster URL secret and do not take the Admin token.
- The guard sits in the API gateway's mux, so the Google front end's path into the gateway is covered too.
