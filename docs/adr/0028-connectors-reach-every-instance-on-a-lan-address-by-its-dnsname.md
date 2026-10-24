# Connectors reach every instance on a LAN address by its dnsName

`--sql-bind` serves every Cloud SQL instance on one LAN address, but the Cloud SQL Auth Proxy and the connectors always dial port 3307 of the address they are given, and on loopback each instance has a Loopback alias of its own for that reason. We decided that every instance reports a `dnsName`, `<id>.<region>.sql.goog.` with an id taken from its connection name, that its server certificate names it, and that port 3307 of a `--sql-bind` address routes each connection to the instance whose `dnsName` the TLS server name is. Connectors send the `dnsName` as the server name and verify the certificate against it, falling back to its CN.

## Considered options

- **One instance per address on port 3307**, as `127.0.0.1` is on a host without Loopback aliases. It leaves loopback untouched, but every other instance would need its own `--sql-bind` address for the connectors to reach it.
- **A `dnsName` only for LAN callers.** Loopback connectors would behave exactly as before, at the price of an instance whose `dnsName` depends on who asks.
- **A name in the Invalid universe** (`<id>.<region>.sql.gcpemu.invalid.`). A connector configured by DNS name would then fail loudly instead of resolving the name against real DNS. But the name only travels inside TLS, and GCP's form is what tools that show or parse it expect.

## Consequences

- Every instance has a `dnsName`, with or without `--sql-bind`, and connectors on loopback send it and verify the certificate's SAN too. An instance created before this gets the name, and a certificate re-issued from its own server CA, at its next start.
- A connector that sends no server name reaches a `--sql-bind` address's instance only when the address serves just one.
- Port 3307 of a `--sql-bind` address stays bound from the first instance served there until the Service stops or is Reset, so the connectors' port does not come and go with instances.
- If the Instance ever serves `sql.goog` names through Cloud DNS or Host mode, the choice of domain should be revisited.
