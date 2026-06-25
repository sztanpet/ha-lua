# State: filesystem plugin (fs-plugin-spec.md)

Working state for the read-only Lua `fs` module. Spec: `fs-plugin-spec.md`.
Global decisions live in `../AI.state`.

Status: **COMPLETE.** Shipped in 1.2.0.

## Filesystem plugin (2026-06-20)
- Read-only Lua `fs` module backed by ONE process-wide `os.Root` rooted at the
  scripts dir, opened in main and shared across all LStates (os.Root is
  goroutine-safe). os.Root rejects symlink/".." escapes at the syscall layer,
  so the bindings pass user paths straight through — no filepath string games.
- API: `fs.read` (8 MiB cap via Stat before read, binary-safe), `fs.exists`
  (bool, never errors), `fs.list` (names via fs.ReadDir(root.FS())), `fs.stat`
  ({size, mtime unix, is_dir}). Error convention `value | nil, errmsg` matching
  http/json. nil root degrades to errors (tests that don't exercise fs pass nil).
- Threading: `RegisterStdlib(L, scriptsDir, root)`, `NewRunner(..., root, ...)`,
  `Deps.Root`. main now does MkdirAll → OpenRoot → Deps{Root} (reordered so the
  dir exists before OpenRoot); `defer scriptsRoot.Close()`.
- `(*os.Root).Close` added to .golangci.yml errcheck exclude (configure the
  tool, don't litter `_ =`) — matches the existing deferred-cleanup list.
- thermostat.lua's ~260-line PAGE long string extracted VERBATIM to
  scripts/thermostat.html, loaded once at load via `assert(fs.read(...))`.
  thermostat.lua: 627 → 367 lines. Tests copy thermostat.html into their temp
  dir and open a test root over it (openTestRoot helper).
- HOT-RELOAD CAVEAT: the watcher only watches .lua; editing the .html alone
  does NOT reload. Documented in the script comment + DOCS.md.
- require loader migrated onto the same os.Root (spec §8/§9.4, 2026-06-20):
  installRestrictedRequire now opens `lib/<mod>.lua` via root.Open + L.Load and
  dropped the lexical filepath.Abs + HasPrefix double-check that could not see
  through symlinks. The cheap `..`/abs guard stays (keeps the "outside
  scripts/lib" error contract); os.Root closes the symlink-escape gap at the
  syscall layer. nil root → require errors "filesystem unavailable".
  TestRequireRejectsSymlinkEscape is the regression guard (lib/evil → outside
  dir; the old lexical check passed it, os.Root rejects it). Test helpers that
  exercise require (newRequireState, newScheduleState, the heating_windows
  Supervisor in scripts_test) now pass a real openTestRoot instead of nil.
- Deferred (in spec §9, do not add without a real use case): write support;
  a dedicated scripts/assets/ root; asset hot reload.
