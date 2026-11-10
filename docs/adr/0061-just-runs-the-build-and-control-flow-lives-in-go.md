# just runs the build, and its control flow lives in Go

Supersedes ADR 0018.

The repository's build, test and release steps are recipes in a root `justfile`, run with just 1.56.0 or later, and `Taskfile.yml` is gone (#932). ADR 0018's goal stands: a contributor on native Windows needs only the tools a recipe itself calls (go, git, pnpm, docker, npm), with no Git Bash, MSYS or coreutils. Task met it with a built-in POSIX shell interpreter, but its Go templating inside YAML (`{{"{{"}}`-escaping, multi-line `cmd: |` scripts) made the shell-heavy tasks (`doctor`, `adr`, `vet`, `image-check`, `echo:image-check`) hard to read and edit. ADR 0018 rejected just because its recipes run in the platform's shell. The answer is that no recipe needs a POSIX shell: control flow moves into Go.

- **Recipes are one plain command per line.** They have no `&&`, `||`, pipes, redirects or POSIX variable syntax, so the same line runs in `sh` and in PowerShell. What a recipe decides, it decides with just's built-ins: `which()`, `env()`, `os()`, `path_exists()`, `error()`, `[env(...)]`, `[working-directory(...)]` and parameterised dependencies in place of Task's `for:`.
- **Anything with control flow is a subcommand of `internal/devtool`**, a stdlib-only Go `main` run as `go run ./internal/devtool <cmd>`: `doctor`, `adr`, `vet`, `image-check`, `echo-image-check`, `docker-arch`, `need-cgo` and `version`. It adds nothing to go.mod, and it is unit-tested like the rest of the code.
- **Windows runs recipes in Windows PowerShell 5.1** (`set windows-shell := ["powershell.exe", "-NoProfile", "-Command"]`), which every Windows install has. Linux and macOS use just's default `sh`.
- **Names are flat and hyphenated** (`echo-dist`, `echo-image-check`, `conformance-record`), and Task's command-line variables become positional parameters with defaults: `just release v1.2.3`, `just image-check gcpemu:check`, `just prerelease colima,orbstack gcpemu:rc`, `just generate latest`. The version is the default of a `version` parameter on `build`, `agent`, `release` and `cross`, so it isn't worked out on every `just` call. A bare `just` lists the recipes.
- Contributors install just themselves. CI pins 1.56.0 with `extractions/setup-just`, and the README states it as the minimum.

## Considered options

- **Keeping Task**, and moving only its long scripts into Go. That fixes the scripts but keeps Go templating in YAML for every variable and condition, and the escaping that came with it.
- **just, with POSIX recipes and Git Bash on Windows.** It reads well, but it gives up ADR 0018's goal.
- **just, with every recipe written twice** (`[unix]` and `[windows]`). Two copies of each recipe drift apart, and only CI's `platforms` job would notice.
- **PowerShell 7 (`pwsh`) as just's Windows shell.** It is nicer to script in, but it isn't installed by default, and recipes no longer need a shell's scripting.
- **Mage, or `go run` for everything.** ADR 0018's objection still holds for one-line commands; `internal/devtool` takes only the logic.

## Consequences

- The migration is one change: `Taskfile.yml` goes, and every reference (CI, error strings, docs, tests) moves to `just`. There is no compatibility shim.
- The first release after it carries a contributor note mapping `task X VAR=v` to `just x v`, as v0.9.0's did for make.
- just has no `sources:`/`generates:` fingerprinting, so incremental builds (#422) stay out of scope.
