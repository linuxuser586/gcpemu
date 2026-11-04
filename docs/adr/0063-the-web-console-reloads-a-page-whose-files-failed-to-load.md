# The Web console reloads a page whose bundle failed to load, and fetches a failed stylesheet again

On Linux, Chromium cancels requests in flight with `net::ERR_NETWORK_CHANGED` whenever the host's network interfaces change, and an Instance changes them itself (ADR 0062). ADR 0062 re-sends the console's own writes, but a page's own files can be cancelled too. When its bundle is cancelled, its document arrives, its module script fails, and the console never starts, so the page stays blank until the user reloads it by hand (#942, #948). When only its stylesheet is cancelled, the console starts anyway, unstyled, and a reload would throw away what the user is doing in it, such as a form's input and the wait for an Operation (#969). We decided that the boot script never reloads a console that has started. `index.html` carries a small inline boot script. When a same-origin `<script>` fails to load, it reloads the page after about 1 s and then 2 s, at most twice per path, counting attempts in `sessionStorage` (cleared once the page's bundle has loaded, or when it gives up; one reload when storage is unavailable). If the last attempt fails too, it writes a static message into `#root` instead of leaving the page blank. When a `<link rel="stylesheet">` fails to load, it adds a new `<link>` for the same file after about 1 s and then 2 s, counting attempts in memory, and if those fail too, it shows a plain-text notice outside `#root`. A failed bundle takes the reload path even when a stylesheet attempt is waiting, because a console that never starts has nothing to lose. The CSP stays `default-src 'self'`: the Web console handler hashes each inline script of the `index.html` it serves, once at startup, and allows exactly those hashes in `script-src`.

## Considered options

- **Re-insert a failed bundle's tag instead of reloading.** It avoids a reload, but a browser may keep a failed module fetch in its module map, so the tag would need a cache-busting query on an asset served as immutable. A stylesheet has no module map, so this reason does not apply to it.
- **Reload for a failed stylesheet too.** This was the first version of this decision. It treats both files alike, but a stylesheet failure leaves the console running, and the reload arrived 1 s later under a form that was being submitted (#969).
- **Delay a stylesheet's reload until the page is idle.** It would need to know about in-flight writes and touched forms, which a boot script outside React cannot see, and an abandoned form would hold the reload off for good.
- **Leave the page unstyled.** It is the simplest option, but an unstyled console looks broken until the user thinks to reload it.
- **Load the boot logic from its own file.** It would need no CSP change, but that file can be cancelled the same way, with nothing left to recover.
- **Write the hash into the build with a Vite plugin.** It adds a generated file that Go must read and keep in step with the build. A hash computed from the served `index.html` cannot drift from it.
- **Allow `'unsafe-inline'`.** It is simpler, but it loosens the CSP for every script the page could ever carry.
- **Fix only the e2e tests.** Users hit the same blank page, so the tests would pass while the console stayed broken, as ADR 0062 also found.

## Consequences

- A page whose bundle failed reloads, and so a deep link opens about 1 s later than it would have. A loader redirect, such as a Build `logUrl` gaining `?project=`, happens after the reload.
- A page whose stylesheet failed keeps running, unstyled for about 1 s or more, and keeps its URL, its input and its requests in flight. If every attempt fails, it stays unstyled until the user reloads it, and the notice says so.
- Any change to the inline boot script changes the CSP the console sends, with no other step needed. Any other inline script added to `index.html` is allowed by the same rule, so `index.html` should hold no inline script except the boot script.
- Failed images and failed `fetch` calls never reload the page.
