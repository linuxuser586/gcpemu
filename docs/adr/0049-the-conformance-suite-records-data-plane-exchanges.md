# The Conformance suite records data-plane Exchanges

ADR 0023 has the Conformance suite record GCP's API responses and compare the emulator's with them. Some Faithful behaviour is not visible in any API response: what a load balancer answers when its URL map's `customErrorResponsePolicy` applies (#855), for one. We decided that a test may also send requests to a data-plane address it created, such as a forwarding rule's IP, and keep each request and its response as an Exchange in its Recording, compared like any other. A data-plane Exchange compares the response's status, its body and only the response headers its test names; the request's method, path and headers are kept for context and never compared. Against GCP, the test first polls the address until a known route answers as expected, since a new load balancer can take minutes to serve, and those polls are left out of the Recording, as Operation polls are. In compare mode the emulator's frontend is on loopback, so the Escape guard still holds.

This keeps the SRS's definition of Faithful, "covered by the Conformance suite", true for data-plane behaviour, and reuses the suite's recording, normalising, sweeping and Drift handling. The cost is that these tests are slow and billed while they run, so a Product that needs them records weekly (ADR 0033), and a header the test does not name can differ unnoticed.

Log fields such as `statusDetails` are not compared: the emulator does not serve the Cloud Logging API (SRS 10.3), so a compare-mode test has no public API to read them through. They are checked against GCP by hand, and the result is kept in the ADR that decided the emulator's value (ADR 0048 for a failed error page fetch).

## Considered options

- **Observe GCP once and list known differences in the SRS.** Cheapest, but nothing would notice Drift or a regression, and the behaviour could no longer be called Faithful.
- **A separate data-plane suite.** It would duplicate the Recording format, normalisation and sweeping for no behaviour the Conformance suite cannot express.
- **Compare every response header, ignoring known noise.** GCP's frontend adds headers (`Via`, `Alt-Svc`, `Date`, `X-Goog-*`) that change without notice, so each new one would fail every Recording.
