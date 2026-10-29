# The Instance CA publishes an empty CRL on the loopback gateway

Windows' Schannel, and so `curl.exe`, refuses a certificate whose revocation it can't check, and the Instance CA's leaves said nowhere where to check: `curl.exe` failed with `CRYPT_E_NO_REVOCATION_CHECK` where Edge and Go accepted the same chain (#843). We decided that every leaf the Instance CA issues carries a CRL distribution point, `http://<loopback>:<gateway port>/ca.crl`, and that the gateway serves an empty CRL there, signed by the Instance CA on each request and valid for 7 days. `<loopback>` is the `--bind` address, or `127.0.0.1` when `--bind` is unspecified, and the port is the one the gateway listened on when the leaf was issued. The Instance CA never revokes anything; the CRL exists only so a client that requires a revocation check can complete one.

## Considered options

- **Documenting `--ssl-revoke-best-effort`** and changing nothing else. It costs nothing, but every Schannel client keeps failing until its user finds the note, and real GCP leaves do publish revocation information.
- **An OCSP responder** that answers "good" for every serial. Schannel accepts a CRL just as well, and OCSP needs request parsing and response signing the standard library doesn't provide.
- **Real revocation**, adding a deleted or replaced certificate's serial to the CRL. It needs persisted CRL state, and a client refused for a revocation the user never sees helps no one testing locally.
- **Serving the CRL to `--gfe-bind` LAN callers too.** It would need a plain-HTTP listener on each LAN address answering without credentials, against `.out-of-scope/anonymous-public-endpoints-on-lan-listeners.md`. Only Windows callers need it, and clients inside the Instance run Linux.

## Consequences

- A CRL that can't be fetched fails Schannel as surely as a missing one (`CRYPT_E_REVOCATION_OFFLINE`), so the URL has to follow the gateway. A stored managed leaf (Certificate Manager, and the load balancer's managed `SslCertificate`s) whose CRL URL is not the Instance's current one is issued again, as one from an older Instance CA already is. That covers leaves from before this decision, a changed gateway port (`--port 0` changes it on every start) and a changed `--bind`. Leaves the Instance holds only in memory are issued again on start anyway.
- A Windows caller on another machine, reaching the Instance through `--gfe-bind`, can't fetch the CRL and still needs `--ssl-revoke-best-effort`.
- Several Instances on one host already need distinct gateway ports or bind addresses, so their CRL URLs are distinct too.
- Self-managed certificates are the user's and carry whatever they carry. Cloud SQL's certificates are signed by each SQL instance's own CAs, not the Instance CA, and are unchanged.
