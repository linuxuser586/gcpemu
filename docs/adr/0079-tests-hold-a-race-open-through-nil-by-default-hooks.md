# Tests hold a race open through hooks that are nil in production

A test that guards a race must be able to hold it open, so that it fails each time with the fix reverted (ADR 0017). Usually a seam the code already has, such as the upstream transport, the Emulator clock or the store, can do that. One of #798's windows sat where none could reach: between two calls in the Artifact Registry's `ptOpenBlob`, with no I/O between them. The 400-goroutine test written for #798 never caught it by itself (#972). We decided that when no existing seam reaches a race's window, the service gets an unexported struct of hook functions. Each hook is nil in production and set only by its tests, and each is called at a named step of what the code does (`missedCache`, `linked`), not named after the bug. The steps exist in both the fixed and the broken code, so the same test runs against both. The tests synchronise through the hooks with channels, not sleeps. A window that the fixed code closes twice over, so that reverting one fix alone loses nothing, gets no hook and no test: such a test could fail only when the other window's test already does.

## Considered options

- **Repeat runs under load.** #798's test passed against the broken code in every one of 20 runs once its socket was removed, and its socket failures were all resets. This gives no evidence either way.
- **Only an external seam, such as a gated transport.** Gating the upstream response holds back the fetch, not a caller, and cannot hold a caller between two calls in `ptOpenBlob`. It does serve the test's other step: holding the fetch until that caller has missed the cache.
- **Sleeps in production code behind a build tag.** These are timing-based in both directions, and a build tag splits the code the tests exercise from the code that ships.

## Consequences

- A service carries fields that nothing in production sets. Each hook struct's doc comment says what it is for and that it is nil in production.
- A hook call costs a nil check on its path.
- Hook steps are internal, so moving the code they mark means moving the hooks and rerunning their tests against the reverted fix.
