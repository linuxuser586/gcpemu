# Anonymous public endpoints on LAN listeners

A `--gfe-bind` LAN listener does not serve Google's public endpoints to
callers without credentials. Without a bearer token the Instance issued,
these all get 401 there, although GCP serves them to anyone:

- `https://www.googleapis.com/oauth2/v1/certs` and `/oauth2/v3/certs`
- `/service_accounts/v1/metadata/{x509,jwk}/EMAIL` and
  `/robot/v1/metadata/{x509,jwk}/EMAIL`
- `/.well-known/openid-configuration`

On loopback (Host mode, the gateway's own address) they are served to
anyone, as before.

## Why this is out of scope

A LAN listener admits only credentials the Instance issued, whatever the
IAM enforcement level (ADR 0011). ADR 0021 made two narrow exceptions on
`--gfe-bind`: the JWT-bearer grant at `oauth2.googleapis.com/token`, and
the Read-through of buckets the Instance doesn't have. Each one is
matched by host, method and path, and each came from a client that
actually broke without it: Argo's key exchange (#462) and the Go module
proxy's downloads.

Nothing has broken for want of anonymous certificates yet. The likely
client is a workload on another machine that verifies emulator-signed ID
tokens or service-account JWTs (`idtoken.Validate`, or a JWT library
fetching a key file's `client_x509_cert_url`) and holds no Instance
credentials. Until one is reported, opening these paths widens the
boundary for no user, and it is not free. The service-account
certificate paths answer 404 for an unknown email and 200 for a known
one, so they would tell any LAN caller which service accounts exist.

A proposal to open them should name the client that fails, say which
paths it needs (the `/oauth2/v{1,3}/certs` paths reveal nothing about
service accounts, and the per-account paths do), and come with an ADR
alongside ADR 0021.

## Prior requests

- #708: "LAN listeners: admit unauthenticated fetches of public certificate endpoints on --gfe-bind"
