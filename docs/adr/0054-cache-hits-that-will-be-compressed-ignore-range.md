# Cache hits that will be compressed ignore Range

With `compressionMode: AUTOMATIC`, a request carrying `Range` was never compressed: the cache answered the range uncompressed. Google documents the opposite: when Cloud CDN compresses a response it sends `Accept-Ranges: none`, and "cache hits for such responses ignore `Range` headers" (#908). We decided that a GET answered from a cached object ignores `Range` and `If-Range` when the response will be compressed, and the client gets 200 with the whole compressed object. "Answered from a cached object" covers every response the cache serves from what it stored: a fresh or stale hit, a revalidated entry, a coalesced follower, and a miss the cache has just stored. A response passed through from the origin (uncacheable, retried after the cache stripped `Range`, or too large to store) keeps its 206 and is not compressed. A response that won't be compressed (the client accepts neither `br` nor `gzip`, or the type or length doesn't qualify) still gets its range.

## Considered options

- **Keeping ranges and skipping compression.** RFC 9110 allows either, and it is kinder to clients that resume downloads, but it is not what GCP does, and a test that passes here would fail against Cloud CDN.
- **Only responses reported as `hit`.** A miss the cache has just stored is answered from the same stored bytes, so it would get a 206 while the next request for the same object got a full 200; the split depends on timing, not on the object.

## Consequences

- A client that sends `Range` with `Accept-Encoding: gzip` or `br` for a compressible cached object gets 200, not 206. It must accept a full response, which RFC 9110 already requires of clients.
- The cache decides whether a response will be compressed before writing it (to drop `Range`), and the compressor decides again when the header is written; both use one rule, so they agree.
- A stored response without a `Content-Type` is answered without one, and so isn't compressed: the cache no longer lets `http.ServeContent` guess a type.
