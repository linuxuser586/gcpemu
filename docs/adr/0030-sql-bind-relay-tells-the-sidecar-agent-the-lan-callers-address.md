# The --sql-bind relay tells the Sidecar agent the LAN caller's address in a signed PROXY header

Amends ADR 0011.

A psql client on another machine reaches a Cloud SQL instance through its `--sql-bind` Postgres port, which the emulator relays from the host to the instance's Sidecar agent. The agent saw the connection come from a container network's gateway or the VM relay, so authorized networks treated the client as a host client: `127.0.0.1/32` admitted every LAN caller, and an entry for the client's own network admitted none (#639). On GCP, authorized networks match the client's address. We decided that the relay writes a PROXY protocol v2 header ahead of the client's bytes, naming the LAN caller's address and carrying an HMAC of it keyed by the instance's agent key. The agent verifies it and reports the address with the login, and the emulator matches that address alone against authorized networks, with no `127.0.0.1` added.

**Only the Postgres port.** The connectors' port 3307 is unchanged: authorized networks do not apply to connector connections, here or on GCP.

**Signed, because the host can reach the agent too.** Any process on the emulator's machine can dial the instance's public IP. An unsigned header from a host gateway would let it claim any LAN address, including one that authorized networks admit while `127.0.0.1/32` is not there. A header that fails the check closes the connection. A connection without a header is matched as before.

**No version check.** The emulator copies the agent into each Postgres container it creates, and recreates every container when it starts, so the relay and the agent are always the same build.

## Considered options

- **Recording each upstream connection's source port** against the client's address, for the login check to look up. It needs no change to the agent, but on a VM runtime the agent sees the VM relay's own port, which the host never learns, and rootless networking can rewrite ports too.
- **An unsigned PROXY header**, trusted when it comes from a host gateway. It is simpler, but it lets host processes impersonate LAN callers.

## Consequences

- A LAN caller, including a client on the emulator's machine that dials the `--sql-bind` address, is matched by its own source address only. Only the loopback host port and the public IP keep host-client matching.
- A LAN caller to an instance with no public IP (`ipv4Enabled=false`) is refused, while a host client is still admitted: a LAN listener serves only what the instance serves on GCP.
- Refusals and the request log name the LAN caller's address, not the gateway's.
- Authorized networks are IPv4, so an IPv6 LAN caller is admitted by no entry. An IPv4-mapped address is matched as IPv4.
