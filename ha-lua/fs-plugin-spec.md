# Filesystem plugin — Specification

> **Working state:** [`state/fs-plugin.md`](state/fs-plugin.md) — implementation progress and decisions.

Status: **implemented** — milestones 1–3 shipped in v1.2.0, milestone 4
(rooted-IO consistency sweep, §9.6) on 2026-07-03. Open decisions are
consolidated in §9.

## 1. Goal

Give Lua scripts a **sandboxed, read-only** way to read files that ship
alongside them — primarily static assets (HTML/CSS/JS) for the script-driven
web UIs (`ha.serve`). Today the only way to serve a page is to embed it as a
giant `[[ ... ]]` long string inside the `.lua` file (see
`scripts/thermostat.lua`, a ~260-line `PAGE` literal). That is the motivating
problem; this plugin is the fix.

The sandbox is Go's `os.Root` (Go 1.24+, we run 1.26.4). `os.Root` confines
every operation to a single directory tree and — unlike the hand-rolled
`filepath.Clean` + `strings.HasPrefix` containment in
`installRestrictedRequire` (`stdlib.go:75-88`) — correctly rejects symlink
escapes and absolute-path traversal at the syscall layer. It is also documented
**safe for concurrent use from multiple goroutines**, which matters because one
`*os.Root` is shared across every per-script LState.

## 2. Why this is needed (the `io` history)

`io`, `loadfile`, `dofile`, and `package` were **deliberately nil'd** from the
sandbox (`stdlib.go:33-53`, AI.state "Lua sandbox" decision). There is
currently *no* file-reading primitive of any kind, by design. This plugin
re-introduces a *minimal, contained* slice of that capability — read-only,
rooted, traversal-safe — rather than re-opening `io`.

## 3. Locked decisions

| Decision | Choice |
|----------|--------|
| Sandbox primitive | **`os.Root`** rooted at the scripts directory. One root, opened once at daemon start, shared across all LStates (it is goroutine-safe). |
| Root location | **`scriptsDir`** (the same dir `RegisterStdlib` already receives, and the parent of `scripts/lib/`). Assets live next to the script that serves them: `scripts/thermostat.html`. |
| Access mode | **Read-only v1.** `fs.read`, `fs.exists`, `fs.list`, `fs.stat`. No write / mkdir / remove (§9.1). |
| Error convention | **`value` on success, `nil, errmsg` on failure** — identical to the `http`/`json` modules (`stdlib_http.go`). Scripts pattern-match `local html, err = fs.read(...)`. |
| Path semantics | Paths are **relative to the root**, `/`-separated, always. A leading `/`, `..`, or any symlink that resolves outside the root → error (enforced by `os.Root`, not by us). |
| Size cap | `fs.read` refuses files larger than **8 MiB** (a Lua string this big is already a smell; assets are KB-scale). Returns `nil, "file too large"`. |

## 4. Lua API

```lua
-- Read an entire file into a Lua string. Binary-safe (Lua strings are bytes).
--   content, err = fs.read("thermostat.html")
-- Returns the contents, or (nil, errmsg) on any error (missing, too large,
-- outside root, is a directory).
local html, err = fs.read("thermostat.html")
if not html then ha.log("asset missing: " .. err) end

-- Cheap existence check. Never raises; returns a boolean.
if fs.exists("overrides.css") then ... end

-- List the entries of a directory (names only, not recursive).
--   names, err = fs.list("assets")
-- Returns an array-table of names (files and subdirs, no "." / ".."), or
-- (nil, errmsg). fs.list(".") lists the root itself.
for _, name in ipairs(fs.list("assets") or {}) do ... end

-- Metadata for one path.
--   info, err = fs.stat("thermostat.html")
-- info = { size = <bytes:int>, mtime = <unix_seconds:int>, is_dir = <bool> }
local info = fs.stat("thermostat.html")
```

| Function | Returns on success | On failure | `os.Root` call |
|----------|--------------------|------------|----------------|
| `fs.read(path)`   | `string` (file bytes) | `nil, errmsg` | `Root.ReadFile` (after a `Stat` size check) |
| `fs.exists(path)` | `boolean`             | — (false, never errors) | `Root.Stat`, `errors.Is(err, fs.ErrNotExist)` |
| `fs.list(path)`   | array-table of names  | `nil, errmsg` | `fs.ReadDir(Root.FS(), path)` |
| `fs.stat(path)`   | table `{size, mtime, is_dir}` | `nil, errmsg` | `Root.Stat` |

Notes:
- **No file handles / streaming.** v1 is whole-file `read` only — no `open`,
  no cursor, no `io`-style object. Keeps the surface tiny and stateless; every
  call is one syscall round-trip, nothing to leak across a hot reload.
- Error messages are the cleaned `error.Error()` (already root-relative from
  `os.Root`); we do not leak absolute host paths.

## 5. Go implementation sketch

New file `internal/lua/stdlib_fs.go`, mirroring `stdlib_http.go`:

```go
func registerFS(L *lua.LState, root *os.Root) {
    // closures capture root; module funcs are plain LGFunctions otherwise
    mod := L.SetFuncs(L.NewTable(), fsFuncs(root))
    L.Push(mod)
    // ... or RegisterModule equivalent
}
```

`root` is threaded through, not global:

- `cmd/ha-lua/main.go` opens it once after `MkdirAll(scripts_dir)`:
  `root, err := os.OpenRoot(cfg.ScriptsDir)` — fatal on error (no scripts dir =
  nothing to run). It lives for the process lifetime; `defer root.Close()`.
- It is passed into the supervisor → `Runner` → `newLState` →
  `RegisterStdlib(L, scriptDir, root)`. `RegisterStdlib`'s signature grows one
  parameter; `registerFS(L, root)` joins the `register*` calls in step 4.

**Why a shared root, not one-per-call:** `os.OpenRoot` holds an open directory
fd; opening one per `fs.read` would be a syscall per call and would re-resolve
the directory each time. The shared root is the same pattern CLAUDE.md mandates
for the SQLite handles ("Two `*sql.DB` handles … shared"). `os.Root` is
explicitly concurrency-safe, so the single instance is correct across every
script goroutine.

## 6. The motivating change: extract the thermostat HTML (Milestone 1)

This is both the first milestone and the acceptance test for the whole plugin.

1. Move the `PAGE = [==[ ... ]==]` long string out of
   `scripts/thermostat.lua` into a new sibling file
   **`scripts/thermostat.html`** (verbatim — no edits to the markup).
2. Replace the literal with a load-time read:

```lua
local PAGE = assert(fs.read("thermostat.html"),
                    "thermostat.html missing next to thermostat.lua")

ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)
```

3. `TestThermostatAPI` (which loads the real script and drives `GET /`) must
   still pass — it is the regression guard that the asset loads and serves
   byte-for-byte.

After this, `thermostat.lua` drops from ~627 lines to ~370, and the HTML is
editable as HTML (syntax highlighting, formatters) instead of as a Lua string.

### 6.1 Hot-reload caveat (must be documented)

The script watcher (`internal/lua/watcher.go`) watches **`.lua` files only**.
With a load-time `fs.read`, editing *just* `thermostat.html` will **not**
trigger a reload — the change is picked up only when `thermostat.lua` itself is
re-saved (or the daemon restarts). For v1 this is acceptable and must be called
out in DOCS.md. Touching the `.lua` after editing the `.html` is the documented
workflow.

Deferred alternative (§9.3): teach the watcher to also watch non-`.lua` files
in the scripts tree and reload the scripts that read them — needs a
script→asset dependency map we do not have today. Out of scope for v1.

## 7. Security posture

- **Containment is `os.Root`'s job, not ours.** No `filepath` string games in
  the Lua bindings — pass the user path straight to `os.Root`, which rejects
  escapes. This is strictly stronger than the `require` loader's current
  hand-rolled check.
- **Read-only.** v1 cannot create, truncate, or delete anything. A buggy or
  hostile script cannot use `fs` to corrupt other scripts, the DB, or host
  files.
- **Root is the scripts dir**, so a script *can* read any other script's
  source and any `lib/*.lua`. For a single-user, single-trust-domain add-on
  (the same trust model as the unauthenticated LAN port in the thermostat UI)
  this is acceptable; stated here so it is a decision, not an accident (§9.2).
- `os.Root`'s documented residual risks (does not block `/proc`, bind mounts,
  or device files *if such a thing were reachable under the root*) do not apply:
  the root is a plain directory under the HA config dir (`/config/ha-lua/scripts`).

## 8. Relationship to `require`

`installRestrictedRequire` (`stdlib.go:68`) predates this plugin and hand-rolls
the same containment problem `os.Root` solves correctly. They should converge:
`require` could resolve `scripts/lib/<name>.lua` through the same `*os.Root`
(via `Root.Open`/`Root.ReadFile` + `L.Load`) and drop its `filepath.Abs` +
`HasPrefix` dance entirely, closing the symlink-escape gap it has today.

That refactor is **out of scope for v1** (it changes a working, tested loader
and earns its own commit), but the plugin is designed so it slots in later:
both would share the one root. Tracked as §9.4.

## 9. Open decisions (consolidated)

### 9.1 Write support — **deferred, default NO**
The only concrete use case is reading assets; `io` was removed on purpose; the
persona is "avoid needless complexity." Ship read-only. Revisit only with a
real write use case (e.g. a script persisting a generated file) — and even then
prefer `store`/`global` for data. `os.Root` already has `Create`/`OpenFile`/
`Mkdir`/`Remove`/`WriteFile` available if/when we say yes.

### 9.2 Root granularity — **default: single root at `scriptsDir`**
Alternative: a dedicated `scripts/assets/` root so scripts can't read each
other's source. Rejected for v1 as needless for a single-trust-domain tool;
revisit if a multi-author scenario ever appears.

### 9.3 Asset hot reload — **default: NO (load-time read only)**
Document the "re-save the `.lua`" workflow. Watcher extension deferred (§6.1).

### 9.4 Migrate `require` onto `os.Root` — **DONE (2026-06-20, commit e1f4438)**
Shipped as Milestone 3 (§10). `installRestrictedRequire` now opens
`lib/<mod>.lua` through the shared `*os.Root` (`root.Open` + `L.Load`) and the
lexical `filepath.Abs` + `HasPrefix` double-check is gone — `os.Root` closes the
symlink-escape gap §8 described. The cheap leading-`..`/abs guard stays in front
to preserve the "lib/ only" contract and the `outside scripts/lib` error
message.

### 9.5 Locked defaults
Read-only; root = `scriptsDir`; error style `nil, errmsg`; read cap 8 MiB;
relative `/`-separated paths; one shared `*os.Root` for the process.

### 9.6 Convert the remaining trusted-path filesystem IO — **DONE (2026-07-03)**
Consistency, not security: the daemon had filesystem-IO sites that used plain
`os` calls on *trusted, non-Lua* paths. They were safe as-is (`os.Root` buys no
containment for a path the user never supplies), but converting the one that
sits under the scripts root keeps a single rooted-IO story. Shipped as
Milestone 4 (§10): `supervisor.LoadAll` now enumerates scripts via
`fs.ReadDir(Root.FS(), ".")`; a nil root fails LoadAll loudly. The `log_file`
write stays blocked on §9.1 (write support); `config`/`watcher` are genuinely
out of scope (their paths live outside the root).

## 10. Implementation milestones

1. **Plugin core + HTML extraction.** `internal/lua/stdlib_fs.go` (`fs.read`,
   `fs.exists`, `fs.list`, `fs.stat`); thread `*os.Root` from `main` →
   `RegisterStdlib`; extract `scripts/thermostat.html` and load it via
   `fs.read`. Unit tests (real `os.Root` over a temp dir: read OK, missing,
   too-large, directory, traversal/`..` rejected, symlink-escape rejected,
   `list`/`stat`/`exists`). `TestThermostatAPI` still green. (One commit —
   tests travel with the code.)
2. **Docs.** DOCS.md `fs` module section + the hot-reload caveat (§6.1);
   CHANGELOG entry. Update the API table.
3. **Migrate `require` onto the shared `os.Root`.** *(DONE, 2026-06-20, commit
   e1f4438.)* `installRestrictedRequire` opens `lib/<mod>.lua` via `root.Open` +
   `L.Load`; dropped the lexical `filepath.Abs` + `HasPrefix` double-check that
   could not see through symlinks. Cheap `..`/abs guard kept (preserves the
   "lib/ only" contract + `outside scripts/lib` message). `nil` root → require
   errors instead of panicking. `TestRequireRejectsSymlinkEscape` is the
   regression guard — the existing require tests pass against the old lexical
   code too, so this is the only test that proves the change. (§8, §9.4.)
4. **Consistency sweep: remaining rooted filesystem IO.** *(DONE, 2026-07-03 —
   §9.6. Done for a single, uniform rooted-IO story, not for security: these
   paths are trusted and never user-supplied.)*
   - `supervisor.LoadAll` — *(done)* replaced `os.ReadDir(s.scriptDir)` with
     `fs.ReadDir(s.deps.Root.FS(), ".")`, so script enumeration goes through
     the same root as `fs`/`require`. The `nil`-root case is a loud LoadAll
     error, not a fallback — main always opens the root, so a nil root at
     LoadAll time is a wiring bug (`TestSupervisorLoadAllNoRoot`). The existing
     supervisor load tests are the regression guard.
   - `exceptions.log_file` — **blocked on §9.1.** This is a *write* to a
     Lua-supplied path; routing it through `os.Root` both needs write support
     (currently a locked NO) and would *restrict* the path to the scripts dir,
     a behavior change. Revisit only when §9.1 is revisited.
   - **Out of scope (not candidates):** `config.go` (`/data/options.json`) and
     `watcher.go` (fsnotify absolute paths) operate *outside* the scripts root;
     `os.Root` cannot express their paths and buys nothing. Recorded here so a
     future pass does not re-investigate them.
