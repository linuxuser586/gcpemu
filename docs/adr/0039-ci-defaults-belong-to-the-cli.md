# CI defaults belong to the CLI, not to config.Defaults()

FR-CI-003 makes `CI=true` default to an Ephemeral Instance, JSON logs and no Web console. `config.Defaults()` applied the first two and `Config.ConsoleEnabled()` read `CI` whenever it was asked, so every caller of either saw the CI defaults: the CLI, but also `emutest` and about thirty test files across the tree. A test that matched text log lines passed locally and failed in CI (#766, #792). We decided that `config.Defaults()` and `ConsoleEnabled()` never read the environment, and the CLI's config loading applies the CI defaults below the config file, env vars and flags, for every `gcpemu` command, `gcpemu run` included. An Instance started by `emutest` does not take them.

## Considered options

- **Keep the CI defaults in `config.Defaults()` and pin each caller**, as #792 did for package `instance` with `testConfig()`. It changes nothing users see, but every new caller has to know to pin, and the next one to forget fails only in CI.
- **Keep `emutest` CI-aware**, so in-process test Instances log JSON and serve no Web console under `CI=true` as `gcpemu` does. It matches the CLI, but a test that passes on a laptop can behave differently in CI, the trap this decision removes, and a test's log output is read by the test's author, not by a log pipeline.

## Consequences

- An `emutest` Instance behaves the same locally and in CI: text logs and the Web console served. `WithConsole` still turns the console on explicitly, but is no longer needed to get it under `CI=true`.
- `Console` is resolved when the CLI loads its config, so a loaded config says whether the console is served; a nil `Console` means on.
- Tests no longer need `t.Setenv("CI", "")` or a text-log pin to be independent of the CI they run in, so package `instance`'s `testConfig()` (#792) and those calls go.
- The CLI's fail-fast under `CI=true` is unchanged: it already reads `CI` in the CLI.
