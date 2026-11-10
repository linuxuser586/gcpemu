# Incremental builds

Every recipe in the `justfile` runs in full each time it is called. just
has no `sources:`/`generates:` fingerprinting to skip a recipe whose
inputs are unchanged since it last ran, and we don't add one in
`internal/devtool`.

## Why this is out of scope

A skipped recipe that should have run leaves a stale artifact that
nothing reports, and for the recipes where a skip would happen in
practice, the time saved is small:

- **`build`, `agent`, `cross`**: Go's build cache already makes an
  unchanged rebuild cheap.
- **`console`**: pnpm and Vite are quick on an unchanged tree, and a
  fingerprint would have to cover the lockfile, every source file and
  the Node and pnpm versions to be right.
- **`action`**: a skip would also skip `npm test`. The recipe is run
  after editing `setup-gcpemu/src`, so its inputs have almost always
  changed, and CI already fails when the committed `setup-gcpemu/dist` is
  stale.
- **`generate`**: it is run deliberately, after moving the
  `google.golang.org/api` version or editing `apis.yaml`, which are the
  very changes that would invalidate a fingerprint. A correct one would
  also have to cover the generator itself (`internal/apidef/internal/apigen`)
  and the pinned googleapis commit it fetches from, and always run when a
  googleapis argument is given: three rules to keep right for a recipe
  that would almost never skip.

CI builds from a clean checkout, so it would never skip anything either;
only a contributor's repeated local builds stand to gain.

## Prior requests

- #422: "Incremental builds in the Taskfile (sources/generates fingerprinting)"
