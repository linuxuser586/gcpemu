# Incremental builds

Every task in `Taskfile.yml` runs in full each time it is called. No task
declares `sources:` and `generates:` (or `status:`) to skip itself when
its inputs are unchanged since it last ran, and there is no `.task/`
checksum directory to clear with `--force`.

## Why this is out of scope

A skipped task that should have run leaves a stale artifact that nothing
reports, and for the tasks where a skip would happen in practice, the
time saved is small:

- **`console`**: a skip here would be silently wrong. The console build
  overwrites the committed placeholder `console/dist/index.html`, and
  `build` and `release` put the placeholder back afterwards, so after
  every build `console/dist/` holds the real `assets/` next to the
  placeholder page. Task only checks that `generates:` files exist, so
  the next `task build` would skip the console and embed the "console not
  built" page with no error.
- **`build`, `agent`, `cross`**: Go's build cache already makes an
  unchanged rebuild cheap.
- **`action`**: a skip would also skip `npm test`. The task is run after
  editing `setup-gcpemu/src`, so its inputs have almost always changed,
  and CI already fails when the committed `setup-gcpemu/dist` is stale.
- **`generate`**: it is run deliberately, after moving the
  `google.golang.org/api` version or editing `apis.yaml`, which are the
  very changes that would invalidate a fingerprint. A correct one would
  also have to cover the generator itself (`internal/apidef/internal/apigen`)
  and the pinned googleapis commit it fetches from, and always run when
  `GOOGLEAPIS` is set: three rules to keep right for a task that would
  almost never skip.

CI builds from a clean checkout, so it would never skip anything either;
only a contributor's repeated local builds stand to gain.

A proposal to fingerprint `console` should first change how the
placeholder works, so that a built bundle and the placeholder can't share
`console/dist/` (for example, building into a directory that isn't
committed, or generating the placeholder instead of committing it), and
come with an ADR.

## Prior requests

- #422: "Incremental builds in the Taskfile (sources/generates fingerprinting)"
