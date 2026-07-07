# ha-lua

Go daemon that connects to the Home Assistant WebSocket API, mirrors all entity state into SQLite, and dispatches events and timer callbacks to per-script Lua VMs (gopher-lua).

## Working process

When writing code, use the persona of Linus Torvalds, and avoid needless complexity.

A feature is never one big commit. Break its development into logical steps,
and **every logical step gets its own git commit in the documented style
below** — not just the final milestone. A logical step is any self-contained,
bisectable unit that compiles and passes `make test` (a schema change, a new
helper, the binding that uses it, the test that covers it). Commit as you go;
do not batch several steps into a single squashed commit at the end.

Follow this sequence for every implementation milestone, and for every logical
step within it:

1. **Search before writing.** Use semble (see below) to find existing patterns, helpers, and conventions in the codebase before introducing new code.
2. **Implement the milestone.** Keep each commit logically complete — tests travel with the code they test, not in a separate commit.
3. **Update the working state.** Record the detail (done/pending, decisions, commit hashes) in the track's `ha-lua/state/<track>.md`, and refresh the single `## Latest` pointer in `ha-lua/AI.state`. See "AI working state" below.
4. **Commit.** Follow the git style below.

### Git commit style

Model after Linus Torvalds' kernel commits: each commit must be a self-contained, bisectable unit of work that compiles and passes `make test`. Never bundle unrelated changes. Never leave a "fix typo in previous commit" in the history — amend or rebase before the work is considered done.

**Subject line** — imperative mood, ≤50 characters:
```
scheduler: add SQLite-backed timer persistence
```

Use a `subsystem:` prefix that matches the primary package changed (`ha`, `state`, `store`, `lua`, `purge`, `scheduler`, `debug`, `config`, `addon`). For cross-cutting changes use the broadest relevant prefix or omit it.

**Body** — wrap at 72 characters, explain *why* not *what*:
```
scheduler: add SQLite-backed timer persistence

Timers registered via ha.every, ha.at, and ha.after are now written
to the timers table. On startup the scheduler loads all rows, fires
any whose next_run is in the past (at most once), and rebuilds the
min-heap.

ha.after timers set from within callbacks are best-effort: if the
process restarts before they fire, the scheduler logs a warning and
deletes the orphaned row rather than silently dropping it.
```

**Rules:**
- Every logical step in a feature's development is its own commit, written in
  the style above. Do not defer committing until the feature is "done."
- Every commit must compile and pass `make test`.
- Tests go in the same commit as the code they test.
- Refactors that are not part of a feature get their own commit.
- Rebase to fix up mistakes; never push a "fix previous commit" to main.
- Use `git rebase -i` to squash or reorder before a milestone is declared done.

## Go package documentation (pkg.go.dev API)

Use the pkg.go.dev REST API to look up package docs, available versions, symbols, and vulnerabilities without leaving the terminal. The API is at `https://pkg.go.dev/v1beta/`.

```bash
# Package metadata (synopsis, version, redistributable, …)
curl -s "https://pkg.go.dev/v1beta/package/github.com/yuin/gopher-lua" | jq .

# Specific version
curl -s "https://pkg.go.dev/v1beta/package/modernc.org/sqlite?version=v1.29.0" | jq .

# All exported symbols (types, funcs, consts, vars)
curl -s "https://pkg.go.dev/v1beta/symbols/github.com/go-json-experiment/json" | jq .

# Available versions for a module
curl -s "https://pkg.go.dev/v1beta/versions/nhooyr.io/websocket" | jq .

# All packages inside a module
curl -s "https://pkg.go.dev/v1beta/packages/golang.org/x/pkgsite" | jq .

# Search
curl -s "https://pkg.go.dev/v1beta/search?q=lua+vm" | jq .

# Known vulnerabilities for a module
curl -s "https://pkg.go.dev/v1beta/vulns/github.com/yuin/gopher-lua" | jq .
```

Full OpenAPI spec: `https://pkg.go.dev/v1beta/openapi.yaml`

---

## Key commands

The Go project lives in the `ha-lua/` add-on subfolder — run all `make`/`go`
commands from there (`cd ha-lua`).

```
make build        # compile to ./ha-lua
make check        # vet + staticcheck + lint + test (CI target)
make test         # go test -race ./...
make lint         # golangci-lint run
make staticcheck  # staticcheck ./...
make fmt          # gofmt -l -w . (all code must be gofmt-clean before commit)
make tidy         # go mod tidy
make hooks        # install the git pre-commit hook (gofmt + vet + staticcheck + lint)
make bench        # run benchmarks → benchmarks/current.txt
make bench-compare # benchstat baseline vs current (informational)
make bench-update  # promote current.txt → baseline.txt
make profile-cpu  # capture 30s CPU profile (needs debug.pprof_addr set)
make trace        # capture 5s execution trace
```

---

## Architecture summary

See `ha-lua/plan.md` for the full design. Short version:

- **One `*lua.LState` per script, owned exclusively by its goroutine.** Never pass an LState across goroutines — gopher-lua is not goroutine-safe.
- **Current state is memory-authoritative; only history is persisted.** The tracker applies each event to an RWMutex-guarded map before dispatch (`ha.get_state` reads only that map), and one background goroutine drains a queue into batched history-append transactions. There is no `states` table (retired after v3.1.0); Seed dedups against memory on reconnect and against the newest history row per entity on cold start. History reads stay on SQLite and may trail the newest event by a beat.
- **Two `*sql.DB` handles per DB file.** Write handle: `SetMaxOpenConns(1)` — serializes all writes, eliminates SQLITE_BUSY. Read handle: default pool — concurrent history/KV reads proceed in parallel. WAL makes this safe.
- **WAL mode** is enabled on every DB open.
- The WS reader goroutine feeds two consumers: the state tracker (fast, synchronous) and the event router (fans out to per-script channels, non-blocking, drops + warns on full).
- Script KV values round-trip via `github.com/go-json-experiment/json` (json/v2) so types (number, boolean, string, table) are preserved.
- Timer IDs are stable across reloads: `script_id|type|spec|N` where N is registration order.
- Every callback dispatch is wrapped in `pcall`; errors are routed to the script's `ha.on_exception` handler, with `slog.Error` as fallback.

---

## Project conventions

- **Script IDs** are the filename without extension (`lights.lua` → `lights`).
- **`store.*`** is scoped per script; **`global.*`** is shared across all scripts. `global` has no proxy — use `global.get/set` directly.
- `store.state(defaults)` returns a persistent-proxy table: reads load from the script's KV store, writes auto-persist as JSON.
- Purge job runs on a ticker (`state_history.purge_interval`). Single DELETE: rows older than `retention_days` (default 2). `Purger.RunOnce()` is exposed for tests.
- Scheduler fires timers via an `onFire` callback → `TimerFiredEvent` → script channel. Scheduler never holds `*lua.LFunction` references.
- `ha.exceptions.email` uses `net/smtp`. Credentials must come from `store.get(...)`, never hardcoded in scripts.
- Restricted `require`: resolves only paths inside `scripts/lib/`. Any other path raises a Lua error.
- **Descriptive variable names in Lua scripts (and their embedded JS).** No single-letter locals (`z`, `c`, `b`, …); name a value for what it holds (`zone`, `climate`, `body`). The idiomatic module table `M`, trivial loop counters (`i`, `d`), and sort comparators (`a, b`) are the only allowed exceptions.
- **CSS: derive colors from other colors with `oklch()`.** When a color is computed from an existing one (lightening, darkening, tinting, deriving a hover/border shade), use `oklch()` (or the `oklch(from …)` relative-color form) rather than hand-picked hex or `hsl`/`rgb`. Standalone base palette values may stay as-is.

---

## Packages

Paths below are relative to the `ha-lua/` add-on subfolder (Go module root).

| Path | Responsibility |
|------|---------------|
| `cmd/ha-lua/` | Entry point, wires all subsystems |
| `internal/ha/` | HA WebSocket client, auth, reconnect, message types |
| `internal/state/` | SQLite schema/migrations, state tracker |
| `internal/store/` | Per-script + global KV over SQLite |
| `internal/lua/` | LState lifecycle, all Lua API bindings, stdlib modules |
| `internal/purge/` | Retention purge goroutine |
| `internal/scheduler/` | SQLite-backed timer engine, catch-up on start |
| `internal/debug/` | Optional pprof/trace HTTP server |
| `internal/config/` | Config loading — `/data/options.json` in prod, YAML via `--config` in dev |
| `internal/testutil/` | `NewTestDB`, seed helpers shared across test packages |
| `internal/e2e/` | End-to-end latency benchmarks: fake HA WS server → full pipeline → `call_service` (test-only; see `event-latency-spec.md`) |

---

## Repository layout (HA add-on repository)

The git root is a Home Assistant **add-on repository**: `repository.yaml` at
the root, and the add-on itself in the `ha-lua/` subfolder (= the Docker build
context, which is why the Go source lives there too). Paths below are relative
to `ha-lua/`.

| File | Purpose |
|------|---------|
| `config.yaml` | HA add-on manifest (options schema, arch, maps) — read by Supervisor, NOT the Go binary |
| `config.dev.yaml` | Standalone config for development outside HA (passed via `--config`) |
| `Dockerfile` | Multi-stage: Go builder → `ghcr.io/home-assistant/base-debian` |
| `run.sh` | Add-on entrypoint: passes `$SUPERVISOR_TOKEN` + fixed WS URL, execs binary |
| `build.yaml` | Multi-arch targets for HA builder |

In production the binary reads **`/data/options.json`** (written by Supervisor). The HA config directory is mounted at **`/config`** (via the `homeassistant_config` map with a `path: /config` override, like the ESPHome add-on), so scripts live at **`/config/ha-lua/scripts/`** — the same path inside the container and on the host, next to `configuration.yaml`. The daemon log is mirrored to **`/config/ha-lua/logs/ha-lua.log`** (and stderr). SQLite DB is at **`/data/ha-lua.db`**.

---

## Release process

Versions follow **SemVer**: a backwards-incompatible Lua API or add-on change
is a **major** bump, new features are **minor**, fixes are **patch**.

The single source of truth for the version is `ha-lua/config.yaml`'s `version:`
field — no other file repeats it. Tag, `config.yaml`, and `CHANGELOG.md` must
all agree before tagging.

Steps for releasing `vX.Y.Z` (do not skip the per-step commits):

1. **Changelog.** Prepend a `## X.Y.Z - YYYY-MM-DD` section to
   `ha-lua/CHANGELOG.md` (Keep a Changelog format: `### Added` / `### Changed`
   / `### Fixed` / `### Security`). Mark breaking changes with a bold
   `**BREAKING: …**` lead. Commit as `docs: changelog for vX.Y.Z`.
2. **Version bump.** Edit only `version:` in `ha-lua/config.yaml`. Commit as
   `release: vX.Y.Z` — config.yaml only, nothing else in that commit.
3. **Tag.** Annotated tag on the `release:` commit:
   `git tag -a vX.Y.Z <release-commit> -m "vX.Y.Z"` (message is just the tag).
   Later docs commits may sit on top of the tagged commit; that's fine.
4. **Update the working state** to record the release: add the version to the `AI.state` release log, refresh the `## Latest` pointer, and note the release detail in the track's `ha-lua/state/<track>.md`.
5. **Push.** There is **no auto-push** — push explicitly. Two remotes:
   `origin` (private mirror) and `github` (github.com). Push `main` and the tag
   to **both**: `git push origin main && git push github main`, then
   `git push origin vX.Y.Z && git push github vX.Y.Z`.

Pushing the `v*` tag to **`github`** triggers `.github/workflows/release.yml`
(at the git root, not in `ha-lua/`), which builds the multi-arch images and
pushes them to GHCR (`ghcr.io/sztanpet/{arch}-ha-lua`). The workflow reads the
version from `config.yaml` at the tagged commit, so the tag must point at a
commit whose `config.yaml` already carries `X.Y.Z`.

---

## AI working state

Claude tracks work state in a **two-level** layout so a session reads only what
it needs, never one giant file of mostly-irrelevant history:

- **`ha-lua/AI.state`** holds ONLY globally-useful data: the single most-recent/
  in-progress thing (a one-paragraph "Latest" pointer to its full state file),
  an index of the per-spec state files, the release log, and the cross-cutting
  "Key decisions / Removed / User preferences" lists. It stays small.
- **`ha-lua/state/<track>.md`** holds the detailed, spec-scoped working state —
  one file per spec (e.g. `state/enhanced-climate.md` for
  `enhanced-climate-spec.md`). Each spec links to its own state file from a
  `> **Working state:** …` line in the spec's header.

**At the start of every session:** read `AI.state` first. Then, only if you are
touching a specific track, read that track's `state/<track>.md`. Do not read the
other tracks' state files.

**When updating after a completed milestone / release:**
1. Put the detail in the relevant `state/<track>.md` (decisions, commit hashes,
   gotchas, what's done/pending for that track).
2. In `AI.state`, keep the **`## Latest`** section to a SINGLE thing — replace
   it with whatever you just worked on, plus the pointer to its state file. Only
   ever one "latest" entry; older context lives in the state files, not here.
3. Add the version to the `AI.state` release log, and add any new cross-cutting
   decision to the "Key decisions" list. Update the state-file index if you
   created a new track.

When a brand-new spec's work begins, create its `state/<track>.md`, add the
`> **Working state:**` link to the spec header, and add the file to the AI.state
index.
