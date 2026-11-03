# The Web console re-sends a write that got no response, if repeating it is safe

On Linux, Chromium cancels requests in flight with `net::ERR_NETWORK_CHANGED` whenever the host's network interfaces change, and an Instance changes them itself each time the container runtime adds or removes a bridge or veth for GKE, Cloud NAT, Cloud Build or Cloud SQL. A cancelled request never reaches the emulator. The console's queries already re-send on a network error, but its mutations did not, so a Create, Save or Delete clicked at the wrong moment failed with "Cannot reach the emulator" (#939, #944). We decided that the console's fetch layer, on both the public API path and the admin API client, re-sends a single request that got no HTTP response at all, up to twice, after about 1 s and then 2 s. A request that got any response, a 5xx included, is never re-sent there. `PUT`, `PATCH` and `DELETE` are re-sent by default. A `POST` is re-sent only when the API function that sends it declares it safe to repeat: a create whose resource name the client chooses, which a duplicate answers with "already exists", or a custom method whose second call changes nothing. A `GET` is left to the queries' existing retry. The emulator honours no request ID, so the only guard against a duplicate is the method and that declaration. The server's answer to a retry is shown as it is.

## Considered options

- **Retry mutations in react-query** (`mutations.retry`). One setting would cover every mutation, but it re-runs the whole mutation. A submit that sends several requests, such as `getPolicy` then `setPolicy`, or `createSecret` then `addVersion`, would start again from its first request and repeat the ones that had succeeded.
- **Re-send every method, `POST` included.** Today's failures were all requests that never left the browser, but the console cannot tell those from a request whose response was lost. Re-sending a publish, a Build `:retry`, a Secret `addVersion` or an `executeSql` could do it twice.
- **Treat a retry's 409, 404 or 412 as success.** The first request may have landed and only its answer been lost. Translating the answer would need a rule per kind of request, for a case we have not seen, and the untranslated answer already shows the resource's real state.
- **Fix only the e2e tests.** Users hit the same cancellation, so the tests would pass while the console stayed broken.

## Consequences

- A `POST` that is neither a create named by the client nor a checked idempotent method fails on the first network error, as before. A `POST` can opt in only after a test shows what a second call does.
- When the first request did land, a re-sent create shows "already exists", a re-sent delete shows "not found", and a write guarded by a fingerprint or etag shows a conflict, even though the change was made.
- A submit that cannot reach the emulator at all reports it about 3 s later than before.
