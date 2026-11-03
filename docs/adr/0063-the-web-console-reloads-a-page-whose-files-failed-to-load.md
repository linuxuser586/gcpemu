# The Web console reloads a page whose files failed to load

On Linux, Chromium cancels requests in flight with `net::ERR_NETWORK_CHANGED` whenever the host's network interfaces change, and an Instance changes them itself (ADR 0062). ADR 0062 re-sends the console's own writes, but a page's bundle can be cancelled too: its document arrives, its module script fails, and the console never starts, so the page stays blank until the user reloads it by hand (#942, #948). We decided that `index.html` carries a small inline boot script that, when a same-origin `<script>` or `<link rel="stylesheet">` fails to load, reloads the page after about 1 s and then 2 s, at most twice per path, counting attempts in `sessionStorage` (cleared once a page's files have all loaded, or when it gives up; one reload when storage is unavailable). If the last attempt fails too, it writes a static message into `#root` instead of leaving the page blank. The CSP stays `default-src 'self'`: the Web console handler hashes each inline script of the `index.html` it serves, once at startup, and allows exactly those hashes in `script-src`.

## Considered options

- **Re-insert the failed tag instead of reloading.** It avoids a reload, but a browser may keep a failed module fetch in its module map, so the tag would need a cache-busting query on an asset served as immutable.
- **Load the boot logic from its own file.** It would need no CSP change, but that file can be cancelled the same way, with nothing left to recover.
- **Write the hash into the build with a Vite plugin.** It adds a generated file that Go must read and keep in step with the build. A hash computed from the served `index.html` cannot drift from it.
- **Allow `'unsafe-inline'`.** It is simpler, but it loosens the CSP for every script the page could ever carry.
- **Fix only the e2e tests.** Users hit the same blank page, so the tests would pass while the console stayed broken, as ADR 0062 also found.

## Consequences

- A page whose files failed reloads, and so a deep link opens about 1 s later than it would have. A loader redirect, such as a Build `logUrl` gaining `?project=`, happens after the reload.
- Any change to the inline boot script changes the CSP the console sends, with no other step needed. Any other inline script added to `index.html` is allowed by the same rule, so `index.html` should hold no inline script except the boot script.
- Failed images and failed `fetch` calls never reload the page.
