# The deterministic clock moves once per API request

In a Deterministic Instance the Emulator clock moves forward one fixed step (1 ms) each time an API request arrives, and every read during that request returns the same instant. Reads anywhere else, such as background loops, timers and startup recovery, never move it. We chose this because the earlier fake clock moved on every read. Any background loop that read it on a real ticker made later timestamps depend on how much real time had passed (#436, #437). Keeping every such loop on a separate non-advancing read was a rule that lived only in comments, and the next new loop would break it with an intermittent CI failure far from the cause (#441).

An API request is a GCP API call that reaches the gateway, a per-service port or a gRPC server; a stream counts once, when it opens. Admin API and console traffic (`/_emu/`), in-container agent callbacks and load balancer or CDN data-plane traffic do not count, because they arrive on wall-clock schedules. `gcpemu time advance` still moves the clock by the requested duration and is not a request step. The guarantee covers clients that send one API request at a time. Operations with no configured latency finish before the response that created them returns, so such a client never polls. Operations backed by runtime containers (GKE clusters, Cloud SQL instances) stay asynchronous and are outside the guarantee, as is any operation once latency is configured.

## Considered options

- **Separate non-advancing reads in background code** (`clock.Peek`, #436). Rejected because nothing enforces it, and much of the sweep code is shared with `ClockAdvanced`, which runs when the clock is advanced on purpose.
- **A read-only clock type for background work.** Rejected for the same shared-code reason, and it still allows timestamps to depend on real time inside requests.
- **Resetting to a per-request base, then ticking within the request.** Rejected because a background read during a request would still shift that request's later timestamps.

## Consequences

Within one request, an operation's start and end times and a resource's creation time can be equal, so anything that sorts by time needs a tie-break. Making a container-backed operation finish synchronously would hold the response open for minutes, which is why those operations are exempt.
