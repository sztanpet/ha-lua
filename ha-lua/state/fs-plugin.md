# State: filesystem plugin (fs-plugin-spec.md)

Working state for the read-only Lua `fs` module. Spec: `fs-plugin-spec.md`.
Global decisions live in `../AI.state`.

Status: **COMPLETE.** Milestones 1–3 shipped in 1.2.0; milestone 4 done
2026-07-03; write support (§9.1) + log-dir-rooted log_file shipped in 3.0.0.

## Write support + log_file confinement (2026-07-03, v3.0.0)
- §9.1 un-deferred on user request. fs.write (create/truncate, NO parent
  mkdir), fs.append, fs.mkdir (MkdirAll), fs.remove (non-recursive) on the
  scripts root; `true | nil, errmsg`. appendToRoot helper shared with the
  exception log.
- ha.exceptions.log_file now writes through a SECOND process-wide os.Root over
  cfg.LogDir — NOT the scripts root (every shipped example logs to
  /config/ha-lua/logs; logs don't belong in the watched scripts tree).
  Threading: main opens it (warn + nil on failure, not fatal) → Deps.LogsRoot
  → NewRunner(…, root, logsRoot, …) → registerExceptionHandlers.
- BREAKING (→ major bump 3.0.0): log_file paths are relative to log_dir.
  Absolute or `..` paths and an unset log_dir (dev default!) raise at
  REGISTRATION time, not at the first exception — a broken error sink must
  fail while someone is looking. All 4 examples updated to relative paths.
- logwriter.RotateIfLarge now takes (*os.Root, path, max) — stat + rename
  through the root. Daemon-log Rotating writer unchanged (its path is
  config-supplied, not Lua-supplied).
- Tests: TestLogFilePathValidation (abs + ..), TestLogFileNoLogsRoot,
  TestLogFileException rewritten relative (reads via runner.logsRoot.Name());
  newHALState + thermostat/enhanced-climate/heating_windows harnesses wire a
  logsRoot openTestRoot. TestFS{Write,Append,Mkdir,Remove} cover the module.

## Milestone 4: rooted-IO consistency sweep (2026-07-03)
- `supervisor.LoadAll` enumerates scripts via `fs.ReadDir(deps.Root.FS(), ".")`
  instead of `os.ReadDir(scriptDir)` — the last read under the scripts dir now
  goes through the same shared os.Root as fs.read and require. Consistency,
  not security (the path was never user-supplied).
- nil-root handling: LoadAll returns an error ("no scripts root"), NOT a
  fallback to os.ReadDir and NOT "no scripts" — main always opens the root, so
  nil at LoadAll time is a wiring bug and must fail loudly.
  TestSupervisorLoadAllNoRoot guards this. The spec offered fallback as an
  option; rejected to avoid a second enumeration path only tests would run.
- Test plumbing: openTestRoot widened *testing.T → testing.TB; newSupervisor
  (supervisor_test.go) now wires Root — it backs LoadAll in the supervisor and
  runner-batch tests. Router tests use StartScript directly, so their nil root
  stays fine.
- Deps.Root doc comment updated: it backs fs, require, AND enumeration.
- log_file stays blocked on §9.1 (write support, locked NO); config/watcher
  remain out of scope (paths outside the root). §9.6 is closed.

## Spec-vs-implementation review (2026-07-03)
Full audit of fs-plugin-spec.md against the shipped code: no functional gaps.
All four bindings, the 8 MiB cap, the nil-root degradation, the shared-root
threading (main → Supervisor Deps → Runner → RegisterStdlib), the require
migration (§9.4), docs (DOCS.md + lua_api.md incl. the §6.1 hot-reload
caveat), and both named regression tests (TestThermostatAPI,
TestRequireRejectsSymlinkEscape) check out; `make check` green. Verified
empirically that os.Root error strings are root-relative (no host-path leak,
spec §4). Two deliberate, correct deviations noted: fs.exists treats ANY
Stat error as false (not just ErrNotExist — matches the "never raises"
contract better than the spec table's errors.Is sketch), and the too-large
error message includes the filename. Milestone 4 (§9.6 trusted-path IO
sweep) remains deferred as specced. Only fixes needed were in the spec doc
itself: stale "draft / ready to build" header → "implemented, shipped in
v1.2.0", and §9.5/§9.6 were out of order. Note: thermostat.lua/.html now
live in examples/ (bundled-examples track) — the spec's scripts/ paths are
historical, left as written.

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
