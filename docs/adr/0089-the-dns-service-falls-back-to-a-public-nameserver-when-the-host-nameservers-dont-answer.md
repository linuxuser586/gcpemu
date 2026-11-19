# The `dns` Service falls back to a public nameserver when the Host nameservers don't answer

An Instance's `dns` Service forwards names outside every zone it serves to the Host nameservers. It used to read them once, at start. On a laptop that had moved networks, the Instance's DNS answered SERVFAIL for every outside name until it restarted, because resolv.conf by then listed two unreachable IPv6 nameservers ahead of a working IPv4 one (#1196). We decided three things:

- **Follow the host.** The Instance reads resolv.conf again when it changes.
- **Pass over dead nameservers.** A Host nameserver that doesn't answer is passed over for 30 s, and every one is tried when all of them are being passed over. The lookup's deadline is shared among the nameservers left to try, so one dead nameserver can't use it all up.
- **Fall back to a public nameserver.** When none of the Host nameservers answer, the Instance asks a public nameserver, Google Public DNS (8.8.8.8), before answering SERVFAIL. It already did this when resolv.conf listed no nameservers at all.

The public fallback is a second exception to NFR-SEC-002 ("the emulator never calls real Google hosts"), beside ADR 0021's Read-through. Like that one, it never carries the Instance's credentials, since a DNS query has none, and it never happens with `--offline`. `--offline` keeps forwarding to the Host nameservers, though: they are how the host's own internal and VPN names resolve, and FR-NAT-005's "tests never hit the internet" is about VPC workloads' egress. `--dns-no-forward` remains the way to stop forwarding altogether.

## Considered options

- **No public fallback.** A host whose nameservers are all briefly dead would leave every VPC workload unable to resolve anything, with no way to recover short of restarting the Instance.
- **A non-Google public nameserver** (1.1.1.1, 9.9.9.9). It would keep NFR-SEC-002's wording but still send lookups to a third party, and the exception would be no smaller.
- **A `--dns-fallback=ADDR|none` flag.** Nobody has asked for it. `--offline` and `--dns-no-forward` already keep lookups off the public nameserver.
- **Watching the platform's network-change events** instead of resolv.conf. That needs code for each OS, and resolv.conf changes when they fire anyway.
- **Querying the nameservers in parallel.** It would send every lookup to every nameserver, the public one included, even when the first Host nameserver would have answered.

## Consequences

- When the Host nameservers are all dead, outside names a workload looks up go to Google Public DNS, including names meant for a VPN that has dropped. To prevent that, use `--offline` or `--dns-no-forward`.
- On a network that blocks outside DNS, a lookup that no Host nameserver answers waits once more, on the public nameserver, before SERVFAIL.
- A containerized Instance's Host nameservers are its container's, usually the runtime's own DNS server, which follows the host on the runtime's side.
- `gcpemu doctor` resolves github.com through a running Instance and, when that fails, warns and names the Host nameservers in use. The warning doesn't fail the run, since a host with no network is a likely cause.
