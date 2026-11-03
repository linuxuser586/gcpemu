# The Web console's bundle never overwrites a committed file

Plain `go build` must work without Node and serve a "console not built" page (FR-UI-001), so `console/dist/index.html` was committed as that page, and `pnpm build` wrote the real bundle over it. The Taskfile put the placeholder back with a `defer:` after every build, and both `release` and `console.Built` told the two apart by a `<meta name="gcpemu-console" content="placeholder">` marker in it. A build that died before the `defer` ran left the tree dirty, the task runner had to support a step that runs on failure, and a bundle sharing `console/dist/` with the placeholder blocks any fingerprinting of the console build (`.out-of-scope/incremental-builds.md`) (#931). We decided that Vite builds into `console/dist/bundle/`, and that the placeholder page is its own committed file, `console/placeholder.html`, embedded beside `dist/`. `console/dist/` keeps one committed file, `.keep`, so `//go:embed all:dist` always has something to embed, and everything else in it is ignored. The console's `FS` serves the bundle when `bundle/index.html` is embedded and the placeholder otherwise, and `Built` means only that `bundle/index.html` is embedded. `release` fails when `console/dist/bundle/index.html` is missing.

## Considered options

- **Keep the placeholder in `dist/` and restore it from Go** (a small helper instead of a `defer:`). It works in any task runner, but a build that is killed still leaves the tree dirty, and the bundle still shares a directory with a committed file.
- **Generate the placeholder at build time** instead of committing it. Plain `go build` runs no generator, so a fresh clone would have nothing to embed.
- **Build the bundle outside `console/`**, for example into `bin/`. `//go:embed` cannot reach outside its package's directory.

## Consequences

- Building the console, or failing to, never changes a tracked file.
- Nothing reads the placeholder's marker any more: `Built` cannot be fooled by a bundle that contains it, or by a placeholder that lacks it.
- A checkout's `console/dist/assets/` and other files from builds before this change stay ignored and are embedded but never served, since `FS` serves only `bundle/`. `git clean -X console/dist` removes them.
- No build step needs Task's `defer:` any more, which the move to just (#932) depends on.
