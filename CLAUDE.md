# ha-lua

Go daemon that connects to the Home Assistant WebSocket API, mirrors all entity state into SQLite (full history + downsampled stats), and dispatches events and timer callbacks to per-script Lua VMs (gopher-lua).

## Key commands

```
make build       # compile to ./ha-lua
make check       # vet + staticcheck + lint + test (CI target)
make test        # go test -race ./...
make lint        # golangci-lint run
make staticcheck # staticcheck ./...
make tidy        # go mod tidy
make profile-cpu # capture 30s CPU profile from running instance (needs debug.pprof_addr set)
make trace       # capture 5s execution trace from running instance
```

## Architecture summary

See `plan.md` for the full design. Short version:

- **One `*lua.LState` per script, owned exclusively by its goroutine.** Never pass an LState across goroutines — gopher-lua is not goroutine-safe.
- **All SQLite writes go through a single `*sql.DB` with `SetMaxOpenConns(1)`.** This serializes writes and avoids SQLITE_BUSY.
- **WAL mode** is enabled on every DB open.
- The WS reader goroutine feeds two consumers: the state tracker (fast, synchronous) and the event router (fans out to per-script channels, non-blocking, drops + warns on full).
- Script KV values round-trip via JSON so types (number, boolean, string, table) are preserved — see `internal/lua/` for the `luaToJSON`/`jsonToLua` helpers.
- Timer IDs are stable across reloads: `script_id|type|spec|N` where N is registration order. Same script, same calls → same SQLite rows preserved.

## Project conventions

- **Script IDs** are the filename without extension (`lights.lua` → `lights`).
- **`store.*`** is scoped per script; **`global.*`** is shared across all scripts.
- `store.state(defaults)` / `global.state(defaults)` return persistent-proxy tables: reads load from SQLite, writes auto-persist. The proxy snapshots at call time — for live cross-script reads use `global.get/set` directly.
- Purge job runs on a ticker (`state_history.purge_interval`). It aggregates numeric history raw→hourly→daily and deletes non-numeric rows beyond retention. `Purger.RunOnce()` is exposed for tests.
- Scheduler fires timers via an `onFire` callback that sends `TimerFiredEvent` to the script's buffered channel. Scripts look up the callback in their local timer map.

## Packages

| Path | Responsibility |
|------|---------------|
| `cmd/ha-lua/` | Entry point, wires all subsystems |
| `internal/ha/` | HA WebSocket client, auth, reconnect, types |
| `internal/state/` | SQLite schema/migrations, state tracker |
| `internal/store/` | Per-script + global KV over SQLite |
| `internal/lua/` | LState lifecycle, all Lua API bindings |
| `internal/purge/` | Downsampling + retention purge goroutine |
| `internal/scheduler/` | SQLite-backed timer engine, catch-up on start |
| `internal/debug/` | Optional pprof/trace HTTP server |
| `internal/config/` | Config loading — `/data/options.json` in prod, YAML via `--config` in dev |

## Add-on layout (repo root = add-on directory)

| File | Purpose |
|------|---------|
| `config.yaml` | HA add-on manifest (options schema, arch, maps) — read by Supervisor, NOT by the Go binary |
| `config.dev.yaml` | Standalone config for development outside HA (passed via `--config`) |
| `Dockerfile` | Multi-stage: Go builder → `ghcr.io/home-assistant/base-debian` |
| `run.sh` | Add-on entrypoint: passes `$SUPERVISOR_TOKEN` + fixed WS URL, execs binary |
| `build.yaml` | Multi-arch targets for HA builder |

In production, the binary reads **`/data/options.json`** (written by Supervisor from user settings). Scripts are at **`/addon_config/scripts/`** inside the container (= `/config/ha-lua/scripts/` on the host). SQLite DB is at **`/data/ha-lua.db`**.

## AI working state

Claude tracks current work state (what is done, what is pending, active decisions) in **`AI.state`**. Read this file at the start of every session before doing anything else.
