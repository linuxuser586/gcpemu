# unzip-stream stub

setup-gcpemu's `package.json` overrides `unzip-stream` with this package.
`@actions/artifact` uses `unzip-stream` only to download artifacts, and the
action only uploads them (diagnostics, in the post step). The real package
depends on `binary` → `buffers@0.1.1`, which declares no licence, so
shipping it fails `just licence-check` (ADR 0090).

The stub is a direct dependency (`file:./stubs/unzip-stream`) and the
override points at it with `$unzip-stream`. Don't drop the dependency or
write the override as a `file:` path: npm resolves a relative `file:`
override against the package that depends on it, `@actions/artifact`,
and leaves a dangling link.

`Parse` and `Extract` throw. `src/bundle.test.js` checks that the bundles
contain none of the real packages and that only `@actions/artifact`'s
download path imports `unzip-stream`.

See [#1202](https://github.com/linuxuser586/gcpemu/issues/1202).
