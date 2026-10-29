# A failed Custom error response fetch is logged as `custom_error_response_fetch_failed`

When a Custom error response's page can't be fetched (it is missing, the error service answers non-2xx, or the error service doesn't exist or can't be reached), the client gets the backend's original status with a short plain-text body, and the access log needs a `statusDetails` for it. GCP documents none (#844). We decided to log `custom_error_response_fetch_failed`, a value the emulator made up, so that a log query can tell a broken error page from a backend that failed.

## Considered options

- **`response_sent_by_backend`**, as for a page served. It hides the broken error page: the log says the backend answered, and the client got a body the backend never sent.
- **`failed_to_connect_to_backend`**, borrowed from the load balancer's own errors. It suggests the matched backend was down, when it answered and only the error service failed.

## Consequences

- Log queries written against the emulator may match a value real GCP never emits. If GCP documents its own value, the emulator switches to it, and those queries change with it.
