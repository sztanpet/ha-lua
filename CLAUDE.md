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
3. **Update `AI.state`.** Mark the milestone done, note any decisions made during implementation, and update the "Pending" list.
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
- **Two `*sql.DB` handles per DB file.** Write handle: `SetMaxOpenConns(1)` — serializes all writes, eliminates SQLITE_BUSY. Read handle: default pool — concurrent reads from multiple script goroutines proceed in parallel. WAL makes this safe.
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

In production the binary reads **`/data/options.json`** (written by Supervisor). Scripts are at **`/addon_config/scripts/`** inside the container (= `/config/ha-lua/scripts/` on the host). SQLite DB is at **`/data/ha-lua.db`**.

---

## AI working state

Claude tracks current work state in **`ha-lua/AI.state`**. Read it at the start of every session before doing anything else. Update it after every completed milestone.
