# The API gateway drops cookies from public API requests

The Web console sent its public API calls and OAuth2 token requests with `credentials: 'omit'`, so behind an Authenticating proxy they lacked the proxy's session cookie, were redirected to sign in on another origin, and failed (#888). Sending same-origin cookies fixes that, but the browser then also sends the Admin token cookie (`Path=/`, ADR 0006) and the proxy's session on every public API call, and the Connect gateway, Delegated API forwarding and Linked Instance forwarding (ADR 0027) copy the incoming `Cookie` header to their backends, so this Instance's Admin token would reach a cluster's kube-apiserver, an External emulator or another Instance. We decided that the console sends same-origin cookies, and that the API gateway deletes `Cookie` from every request that is not an Admin API or Web console request, first thing at each of its entry points: `ServeHTTP`, and `ServeLinked` for requests a Linked Instance forwarded, which are never Admin API or Web console requests and lose `Cookie` outright. No public API reads cookies: a Principal comes only from an access token.

A request is an Admin API or Web console request by its host as well as its path. Host mode's hostnames serve only public APIs, so a request whose `Host` is one of them (a Service's or Delegated API's host, or `www.googleapis.com`) loses `Cookie` whatever its path, and `/_emu/…` and `/console…` there are public API paths that never reach the Admin API or the Web console, nor need the Admin token (#890). Otherwise a Delegated API's host forwarded `firestore.googleapis.com/_emu/…` to its External emulator with the Admin token cookie, and `www.googleapis.com` served the console.

## Considered options

- **Stripping `Cookie` in each forwarding proxy.** Every new forwarding path would have to remember it, and the LAN Read-through proxy, the only one that drops it today, shows how easily the others were missed.
- **Keeping `credentials: 'omit'` and leaving proxies unsupported.** The console works on loopback but not behind oauth2-proxy, the usual way to share it beyond loopback with sign-in.
- **`credentials: 'include'` for a proxy on another origin.** It needs CORS with credentials on every public API. Not supported.
- **Checking the Admin token before Host mode's dispatch.** It would demand the Admin token of a public call such as `storage.googleapis.com/console/obj`, an object in a bucket named `console`, and still forward the cookie once the token was good.

## Consequences

- A cookie-based login for a public API, as real Cloud Storage offers on `storage.cloud.google.com`, would have to undo this for its host.
- Load balancer session affinity cookies are unaffected: the load balancers' frontends serve them, not the API gateway.
- A Service handler never sees `Cookie`; a test that sets one on a public API request through the API gateway sees it removed.
- The `--gfe-bind` LAN listener stays stricter (ADR 0021): it answers `/_emu/…` and `/console…` with Not Found on every hostname, so a bucket named `console` is reachable through loopback Host mode but not from the LAN.
