# Task runs the build, not make

The repository's build, test and release steps are tasks in a root `Taskfile.yml`, run with Task (go-task), and there is no Makefile. Task runs every command through its built-in POSIX shell interpreter, so the same recipes run from PowerShell, cmd, zsh or bash. The aim is that a contributor on native Windows needs only the tools a task itself calls (go, git, pnpm, docker, npm), with no Git Bash, MSYS or coreutils. Since ADR 0015 made native Windows a full platform for the emulator, asking its contributors to build from a POSIX userland was the last place where Windows was second-class.

- Recipes call no Unix-only programs, such as `grep`, `sed` or `awk`. Logic that make did in the shell uses Task's own `for:`, `preconditions:` and `defer:`, or shell builtins the interpreter provides. CI on Windows runs the tasks from pwsh, so a recipe that quietly depends on a POSIX tool on PATH fails there.
- Task names and command-line variables match the old make targets (`task build`, `task release VERSION=…`, `task generate GOOGLEAPIS=latest`), and a bare `task` runs `build`.
- Contributors install Task themselves. CI pins an exact version, and the README states the version we test against as the minimum.

## Considered options

- **Keeping make, and running it from Git Bash on Windows.** This is what we did before. It works, but it ties the Windows build to a POSIX userland that isn't part of the product's requirements, and GNU make on Windows quietly uses cmd.exe when no `sh` is on PATH.
- **Mage, or plain `go run` build scripts.** These run everywhere Go does, but every one-line command becomes Go code to maintain, and their command lines are less familiar than a task runner's.
- **just.** It has the same model as Task, but its recipes run in the platform's shell, so Windows still needs `sh` for POSIX recipes, or each recipe has to be written twice.
- **Running Task as `go tool task` from go.mod.** That needs nothing besides Go, but it pulls Task's dependency tree into the emulator's go.mod and go.sum.
