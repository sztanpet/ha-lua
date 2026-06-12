# ha-lua: Home Assistant Lua Scripting Engine

A Go daemon that connects to the HA WebSocket API, mirrors all state into SQLite (full history), and dispatches events to registered Lua scripts.

---

## Architecture Overview

```
HA WebSocket
     │
     ▼
 WS Reader goroutine
     │
     ├──► State updater  (upserts states + appends state_history)
     │
     └──► Event router
               │
               ├──► Script[0] goroutine  (own LState + event channel)
               ├──► Script[1] goroutine  (own LState + event channel)
               └──► Script[N] goroutine  (own LState + event channel)
```

Key invariant: **one `LState` per script, owned exclusively by that script's goroutine**. `gopher-lua` `LState` is not goroutine-safe. Scripts never share state; errors in one script don't affect others.

---

## Go Libraries

| Purpose | Library |
|---------|---------|
| WebSocket | `nhooyr.io/websocket` |
| Lua VM | `github.com/yuin/gopher-lua` (Lua 5.1, pure Go) |
| SQLite | `modernc.org/sqlite` (pure Go, no cgo) |
| File watching | `github.com/fsnotify/fsnotify` |
| Config | `gopkg.in/yaml.v3` |
| JSON | `github.com/go-json-experiment/json` (json/v2 — will become `encoding/json/v2` in stdlib once the Go proposal lands; import path swap only when that happens) |

---

## Directory Structure

```
ha-lua/
├── cmd/
│   └── ha-lua/
│       └── main.go           # entry point, wires everything together
├── internal/
│   ├── testutil/
│   │   └── db.go             # NewTestDB, seed helpers shared across test packages
│   └── ...
│   ├── ha/                   # WebSocket client + HA message types
│   │   ├── client.go
│   │   ├── types.go
│   │   └── reconnect.go
│   ├── state/                # SQLite state tracker
│   │   ├── db.go             # schema, migrations
│   │   └── tracker.go        # upsert states, append history
│   ├── lua/                  # Lua VM lifecycle + API bindings
│   │   ├── runner.go         # per-script goroutine, LState ownership
│   │   ├── api_ha.go         # ha.* API (get_state, call_service, on_exception, exceptions.*)
│   │   ├── api_store.go      # store.* and global.* API
│   │   ├── registry.go       # event routing table
│   │   ├── json.go           # luaToJSON / jsonToLua helpers (uses go-json-experiment/json)
│   │   ├── stdlib.go         # RegisterStdlib entry point + sandboxing
│   │   ├── stdlib_time.go    # time module + time userdata metatable
│   │   ├── stdlib_strings.go # strings module
│   │   ├── stdlib_json.go    # json module (delegates to json.go)
│   │   ├── stdlib_re.go      # re module + per-LState regex cache
│   │   ├── stdlib_http.go    # http module
│   │   ├── stdlib_crypto.go  # crypto module (hash, hmac, base64, hex, rand)
│   │   └── stdlib_math.go    # math augmentation
│   ├── store/                # per-script + global KV (thin wrapper over SQLite)
│   │   └── kv.go
│   ├── purge/                # retention purge goroutine
│   │   └── purge.go
│   ├── scheduler/            # SQLite-backed timer engine
│   │   └── scheduler.go
│   ├── debug/                # optional pprof/trace HTTP server
│   │   └── pprof.go
│   └── config/
│       └── config.go
├── benchmarks/
│   ├── baseline.txt          # committed; updated with `make bench-update`
│   └── .gitignore            # ignores current.txt
├── scripts/                  # user Lua scripts, edited via Studio Code Server
├── tools.go                  # //go:build tools — pins staticcheck + golangci-lint
├── Makefile
├── .golangci.yml
├── config.yaml               # HA add-on manifest (options schema, arch, maps)
├── config.dev.yaml           # standalone config for development outside HA
├── Dockerfile                # multi-stage: Go builder → HA base image
├── run.sh                    # add-on entrypoint: reads options, execs binary
├── build.yaml                # multi-arch build targets for HA builder
├── DOCS.md                   # user-facing add-on documentation
├── CHANGELOG.md
├── icon.png
├── logo.png
└── plan.md
```

---

## HA WebSocket Lifecycle

### Auth flow (on every connect/reconnect)
1. Receive `{"type": "auth_required"}`
2. Send `{"type": "auth", "access_token": "..."}`
3. Receive `{"type": "auth_ok"}` — proceed; `"auth_invalid"` — fatal

### Seed on connect
After auth, send `get_states` and upsert all returned states into both `states` and `state_history` (with `seeded_at` marker to distinguish from real changes if needed).

### Subscriptions
Always subscribe to `state_changed`. For `ha.on_event` registrations, collect the distinct set of event types across all loaded scripts and subscribe to each one explicitly — never subscribe to `*`. Subscriptions are sent after re-auth on every connect/reconnect.

```json
{"id": 1, "type": "subscribe_events", "event_type": "state_changed"}
{"id": 2, "type": "subscribe_events", "event_type": "custom_event_type"}
```

On hot reload, subscribe to any newly required event types. Do not unsubscribe from types that are no longer referenced — the HA protocol does not support per-subscription cancel cleanly, and the overhead of receiving an event nobody handles is negligible compared to the complexity of tracking active subscription IDs.

### Reconnect with backoff
- Exponential backoff: 1s → 2s → 4s → … → 60s cap
- On reconnect: re-auth → re-seed → re-subscribe
- Scripts are **not** reloaded on reconnect; their goroutines keep running

---

## SQLite Schema

WAL mode enabled on open. Two `*sql.DB` handles are opened against the same file:

- **Write handle** — `SetMaxOpenConns(1)`. All INSERT/UPDATE/DELETE go through this handle. The single connection serializes writes and eliminates SQLITE_BUSY entirely.
- **Read handle** — default connection pool (no `SetMaxOpenConns` cap). Concurrent `SELECT` from multiple script goroutines go here. WAL guarantees readers never block the writer and the writer never blocks readers — the two handles exploit this properly.

Using a single handle with `SetMaxOpenConns(1)` would serialise reads too, throwing away the only reason WAL exists.

```sql
-- Current state mirror (for fast get_state lookups)
CREATE TABLE IF NOT EXISTS states (
    entity_id   TEXT PRIMARY KEY,
    state       TEXT NOT NULL,
    attributes  TEXT NOT NULL DEFAULT '{}',  -- JSON
    last_changed TEXT NOT NULL,
    last_updated TEXT NOT NULL
);

-- Full history log (append-only)
CREATE TABLE IF NOT EXISTS state_history (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id   TEXT NOT NULL,
    state       TEXT NOT NULL,
    attributes  TEXT NOT NULL DEFAULT '{}',  -- JSON
    changed_at  TEXT NOT NULL                -- RFC3339
);
CREATE INDEX IF NOT EXISTS idx_sh_entity_time ON state_history(entity_id, changed_at);

-- Per-script key-value store
CREATE TABLE IF NOT EXISTS script_kv (
    script_id   TEXT NOT NULL,
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,  -- JSON-encoded (preserves number/bool/string/table types)
    PRIMARY KEY (script_id, key)
);

-- Global key-value store (shared across all scripts)
CREATE TABLE IF NOT EXISTS global_kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL  -- JSON-encoded
);
```

`states` keeps only the latest row per entity (fast `get_state`). `state_history` is append-only. Both are written in a single transaction per `state_changed` event. Old rows are deleted by the purge job after `retention_days`.

---

## Script Registration Model

Scripts register handlers **in-script** (AppDaemon-style). The Lua file is loaded once; the `ha.on_*` calls at the top level register callbacks. No separate manifest file.

```lua
-- scripts/lights.lua

-- Persistent per-script state: loaded from store on every (re)load, auto-saved on write.
local state = store.state({ counter = 0, last_entity = "" })

ha.on_state_change("light.*", function(event)
    local new_state = event.new_state.state
    ha.log("info", "light changed to " .. new_state)
    if new_state == "on" then
        state.counter     = state.counter + 1        -- auto-persisted
        state.last_entity = event.data.entity_id     -- auto-persisted
        global.set("total_activations", (global.get("total_activations") or 0) + 1)
        ha.call_service("notify", "mobile_app", { message = "Light turned on" })
    end
end)

ha.on_event("custom_event_type", function(event)
    ha.log("debug", "got custom event")
end)

-- Error handler: email on any unhandled Lua error in this script
ha.on_exception(ha.exceptions.email({
    to        = "user@example.com",
    smtp_host = "smtp.gmail.com",
    smtp_port = 587,
    username  = "user@gmail.com",
    password  = store.get("smtp_password"),  -- credentials via KV, never hardcoded
}))
```

`script_id` is the filename without extension (e.g. `lights`). The KV store is automatically scoped to that ID.

### `scripts/lib/` shared utilities

`require` is re-enabled in a restricted form: it only resolves paths inside `scripts/lib/`. This lets scripts share common helpers without duplicating code.

```lua
-- scripts/lib/notify.lua
local M = {}
function M.alert(msg) ha.call_service("notify", "mobile_app", { message = msg }) end
return M

-- scripts/lights.lua
local notify = require("notify")   -- loads scripts/lib/notify.lua
notify.alert("hello")
```

Any `require` path that resolves outside `scripts/lib/` raises a Lua error. The restricted loader is installed at LState creation time, replacing the default `require`.

---

## Lua API

### `ha` module

| Function | Description |
|----------|-------------|
| `ha.on_state_change(pattern, fn [, opts])` | Register callback for `state_changed` events where `entity_id` matches glob pattern. `opts.initial = true` immediately calls `fn` for every currently-matching entity (with `old_state = nil`) so the script is in sync from the first moment. Load-time only. |
| `ha.on_event(event_type, fn)` | Register callback for any HA event type. Load-time only. |
| `ha.on_exception(handler)` | Register a single error-handler function for this script. Called whenever a callback raises an unhandled Lua error. Load-time only. See **Error Handling** section. |
| `ha.get_state(entity_id)` | Returns `{state, attributes}` table from the `states` mirror. |
| `ha.get_entities(pattern)` | Returns array of `{entity_id, state, attributes}` for all entities whose ID matches the glob. Queries the `states` mirror. |
| `ha.get_entity_ids(pattern)` | Returns array of entity ID strings matching the glob. Cheaper than `get_entities` when attributes are not needed. |
| `ha.get_history(entity_id, since, limit)` | Returns array of `{state, attributes, changed_at}` from `state_history`. `since` is ISO8601 string, `limit` is int. |
| `ha.call_service(domain, service, data)` | Calls a HA service. `data` is a Lua table, serialized to JSON. |
| `ha.fire_event(event_type, data)` | Fires a HA event. |
| `ha.log(level, message)` | Logs at level `"debug"`, `"info"`, `"warn"`, `"error"`. |

### `ha.exceptions` built-in handlers

| Factory | Description |
|---------|-------------|
| `ha.exceptions.email(config)` | Returns a handler that sends a plain-text email via `net/smtp`. `config`: `to` (string or array), `smtp_host`, `smtp_port`, `username`, `password`, optional `from` and `subject_prefix`. |
| `ha.exceptions.log_file(path)` | Returns a handler that appends a timestamped plain-text error entry (error, traceback, triggering event JSON) to `path`. |

Both factories are pure Go, backed by `net/smtp` and `os.OpenFile` respectively. Credentials in `ha.exceptions.email` should always come from `store.get(...)` rather than being hardcoded in the script.

### `store` module (scoped to script)

| Function | Description |
|----------|-------------|
| `store.get(key)` | Returns stored value (string/number/boolean/table) or `nil`. |
| `store.set(key, value)` | Persists value; accepts string, number, boolean, or table. |
| `store.delete(key)` | Removes key. |
| `store.get_all()` | Returns table of all key→value pairs for this script. |
| `store.state(defaults)` | Returns a **persistent-proxy table**. Each key is loaded from the store on call (falling back to `defaults`). Any assignment to the proxy auto-persists to the store. See below. |

### `global` module (shared across all scripts)

| Function | Description |
|----------|-------------|
| `global.get(key)` | Returns stored value or `nil`. Reads live from SQLite. |
| `global.set(key, value)` | Persists value to the global store. |
| `global.delete(key)` | Removes key from the global store. |
| `global.get_all()` | Returns table of all global key→value pairs. |

### Persistent-proxy table (`store.state`)

`store.state(defaults)` returns a Lua table wired with `__index` / `__newindex` metamethods. At call time, all keys are loaded from the script's KV store (one `SELECT` for the script's rows) and decoded into an internal Go map hidden from Lua. Defaults fill in any missing keys. After that, **reads serve from the in-memory Go map — no SQLite on read**. On any assignment the value is JSON-encoded, written to SQLite immediately, and the in-memory map is updated. There is no round-trip to SQLite on read; the map is the cache.

Types are preserved: numbers stay numbers, booleans stay booleans, strings stay strings, nested tables are supported. Only available for per-script state — the global module has no equivalent proxy, by design.

```lua
local s = store.state({ counter = 0, label = "hello", active = true })

-- All three are loaded from the store (or from defaults on first run):
print(s.counter)   -- number
print(s.label)     -- string
print(s.active)    -- boolean

-- Assignments persist automatically:
s.counter = s.counter + 1
s.label   = "world"
s.active  = false
```

### Event table shape (passed to callbacks)

```lua
event = {
    event_type = "state_changed",
    time_fired = "2026-01-01T00:00:00Z",
    data = {
        entity_id = "light.living_room",
        old_state = { state = "off", attributes = {...} },
        new_state  = { state = "on",  attributes = {...} },
    }
}
```

---

## Per-Script Goroutine Model

```
Script goroutine owns:
  - *lua.LState         (never shared)
  - chan Event           (buffered, e.g. 64)
  - context.Context     (for shutdown + per-call timeout)

On event dispatch:
  router sends Event to script's channel (non-blocking, drop + warn if full)

Script goroutine loop:
  for event := range ch {
      ctx, cancel := context.WithTimeout(parent, 5s)
      lstate.SetContext(ctx)
      call registered callbacks matching this event
      cancel()
  }
```

Per-call timeout via `LState.SetContext` prevents a blocked script from stalling its queue indefinitely.

---

## Error Handling

Every callback dispatch is wrapped in `L.CallByParam` with deferred Go-level recovery. If the Lua call returns an error:

1. Collect the error message and stack traceback from gopher-lua.
2. Build an **exception info table** and pass it to the script's registered `on_exception` handler (if any).
3. If no handler is registered, or if the handler itself errors, log the full details via `slog.Error` and continue — the goroutine keeps running.

### Exception info table

```lua
{
    script_id   = "lights",
    error       = "attempt to index nil value (field 'state')",
    traceback   = "stack traceback:\n\t[string \"lights.lua\"]:42: in function ...",
    callback    = "state_changed",   -- "state_changed" | "event" | "timer_every"
                                     -- | "timer_at" | "timer_after"
    event       = { ... },           -- triggering event table; nil for ha.every / ha.at
    timestamp   = "2026-01-01T12:00:00Z",
}
```

`traceback` is produced by gopher-lua's internal traceback before the debug library is removed from the global environment (the removal only affects script-level access, not internal Go calls).

### `ha.exceptions.email` — implementation notes

Uses `net/smtp.SendMail`. The email body is a plain-text template:

```
Script:   lights
Time:     2026-01-01T12:00:00Z
Callback: state_changed

Error:
  attempt to index nil value (field 'state')

Traceback:
  [string "lights.lua"]:42: in function <lights.lua:38>
  ...

Triggering event:
  {"event_type":"state_changed","data":{"entity_id":"light.bedroom", ...}}
```

Subject defaults to `[ha-lua] Error in script: lights`.

### `ha.exceptions.log_file` — implementation notes

Appends to the file using `os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)`. Each entry is separated by `---\n`. Format mirrors the email body above.

---

## Hot Reload

`fsnotify` watches `scripts_dir`. On `WRITE` or `CREATE` event for a `.lua` file:

1. Find the running script goroutine by script ID.
2. Signal it to drain and stop (close its channel or send a sentinel).
3. Wait for goroutine to exit.
4. Re-parse the file, create a new `LState`, re-register callbacks.
5. Spawn a new goroutine for the script.

Script KV data in SQLite persists across reloads automatically.

---

## Config

### Production (HA add-on)

When running as an add-on, the HA Supervisor writes user settings to `/data/options.json`. The binary reads this file at startup. No separate config file is needed. The HA URL is always `ws://supervisor/core/websocket` and the token comes from `$SUPERVISOR_TOKEN`.

### Development (`config.dev.yaml`)

For running outside HA (standalone development), a YAML file is used. Pass the path via `--config` flag; the binary falls back to `/data/options.json` if the flag is absent.

```yaml
homeassistant:
  url: "ws://homeassistant.local:8123/api/websocket"
  token: "your_long_lived_access_token"

scripts_dir: "./scripts"
database: "./ha-lua.db"
log_level: "info"

state_history:
  retention_days: 2    # raw history kept for this many days, then deleted
  purge_interval: "1h" # how often the purge job runs

debug:
  pprof_addr: ""
```

`internal/config/config.go` tries `/data/options.json` first; if absent or the `--config` flag is set, loads the YAML file instead. Both map to the same internal `Config` struct.

---

## Purge Job

`internal/purge/purge.go` — a `Purger` struct with `New(db, cfg)` and `Start(ctx)`. `Start` spawns a goroutine with a `time.Ticker` at `purge_interval`. Expose `RunOnce()` for tests.

Each tick runs a single DELETE:

```sql
DELETE FROM state_history
WHERE changed_at < datetime('now', '-' || ? || ' days');
```

Parameter is `retention_days` (default 2). HA's own recorder/statistics handles any long-term history needs; `state_history` here is for short-window queries from scripts via `ha.get_history`.

---

## Go Tooling

### `tools.go`

Pins pure-Go tool versions in `go.sum` via the `//go:build tools` pattern:

```go
//go:build tools

package tools

import (
    _ "honnef.co/go/tools/cmd/staticcheck"
    _ "golang.org/x/perf/cmd/benchstat"
)
```

`golangci-lint` is **not** pinned here. It has C dependencies (some of its analysers link against system libraries) and `go install` is unreliable across aarch64/amd64 with CGO disabled. Install it via the official script:

```bash
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sh -s -- -b $(go env GOPATH)/bin v1.64.0
```

In CI use the official `golangci/golangci-lint-action` GitHub Action, which caches the binary for the correct platform automatically. Pin the version in both places.

### `Makefile`

```makefile
BIN     := ha-lua
GOFLAGS := -trimpath -ldflags="-s -w"

BENCH_BASELINE := benchmarks/baseline.txt
BENCH_CURRENT  := benchmarks/current.txt
BENCH_FLAGS    := -run='^$$' -bench=. -benchmem -count=5

.PHONY: build test bench bench-update bench-compare vet staticcheck lint check tidy profile-cpu trace

build:
	go build $(GOFLAGS) -o $(BIN) ./cmd/ha-lua

# Unit tests — primary correctness target
test:
	go test -race ./...

# Run benchmarks and save to current.txt
bench:
	go test $(BENCH_FLAGS) ./... | tee $(BENCH_CURRENT)

# Show benchmark delta vs committed baseline (informational, does not fail the build)
bench-compare: bench
	@if [ -f $(BENCH_BASELINE) ]; then \
	    benchstat $(BENCH_BASELINE) $(BENCH_CURRENT); \
	else \
	    echo "WARN: no benchmark baseline; run 'make bench-update' to create one."; \
	fi

# Overwrite the committed baseline (run after intentional perf improvements)
bench-update: bench
	cp $(BENCH_CURRENT) $(BENCH_BASELINE)

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

lint:
	golangci-lint run

check: vet staticcheck lint test

tidy:
	go mod tidy

profile-cpu:
	go tool pprof -http=:8080 "http://localhost:6060/debug/pprof/profile?seconds=30"

trace:
	curl -s "http://localhost:6060/debug/pprof/trace?seconds=5" -o trace.out
	go tool trace trace.out
```

### `.golangci.yml`

```yaml
linters:
  enable:
    - staticcheck   # SA*, S*, QF* — supersedes gosimple + unused as standalone
    - errcheck
    - govet
    - ineffassign
    - misspell
    - noctx
    - exhaustive
    - goimports

linters-settings:
  staticcheck:
    checks: ["all"]

issues:
  max-same-issues: 0
  exclude-use-default-excludes: false
```

### `internal/debug/pprof.go`

When `debug.pprof_addr` is non-empty, starts an HTTP server on that address. Enables block and mutex profiling so goroutine contention is visible. Shuts down cleanly when the root `context` is cancelled.

Endpoints provided by `net/http/pprof` (imported for side effects):
- `/debug/pprof/profile?seconds=N` — CPU profile
- `/debug/pprof/heap` — heap snapshot
- `/debug/pprof/goroutine` — goroutine dump
- `/debug/pprof/trace?seconds=N` — execution trace

---

## Scheduling & Timers

HA automations are frequently time-driven. Timers are **persisted in SQLite** so any timer that did not fire during downtime is caught up immediately on startup (fired once, then rescheduled normally — not replayed multiple times).

### Schema addition

```sql
CREATE TABLE IF NOT EXISTS timers (
    id        TEXT NOT NULL PRIMARY KEY,  -- stable: script_id|type|spec|index
    script_id TEXT NOT NULL,
    type      TEXT NOT NULL,              -- "every" | "at"
    spec      TEXT NOT NULL,              -- "5m" / "1h30m" | "08:00"
    last_run  TEXT,                       -- RFC3339, NULL = never run
    next_run  TEXT NOT NULL               -- RFC3339
);
CREATE INDEX IF NOT EXISTS idx_timers_next ON timers(next_run);
```

`id` is derived as `script_id|type|spec|N` where N is the 1-based registration order within the script. Same script, same calls in the same order → same IDs → SQLite rows preserved across reloads.

On script load: `INSERT OR IGNORE` (preserve existing `last_run`/`next_run`). After registration, delete any timer rows for this script that were not just registered (orphaned from removed calls).

### Lua API

| Function | Description |
|----------|-------------|
| `ha.every(interval, fn)` | Fire `fn` every `interval` (duration string: `"5m"`, `"1h"`, `"30s"`). Fires immediately on startup if the last run was more than `interval` ago. Load-time only. |
| `ha.at(time, fn)` | Fire `fn` daily at `time` (`"HH:MM"` in local time). Fires immediately on startup if it has not run today yet. Load-time only. |
| `ha.after(interval, fn)` | Fire `fn` once after `interval`. Can be called from callbacks as well as at load time. **Best-effort persistence**: if called from a callback and the process restarts before the delay expires, the callback cannot be recovered and the timer row is deleted on startup. See note below. |

`ha.every` and `ha.at` are valid **only at script load time** (top level). `ha.after` may be called anywhere, including inside event/timer callbacks.

```lua
ha.every("5m", function()
    ha.log("info", "5-minute tick")
end)

ha.at("07:00", function()
    ha.log("info", "good morning")
    ha.call_service("light", "turn_on", { entity_id = "light.bedroom" })
end)

-- One-shot: turn off the light 30 minutes after it was switched on
ha.on_state_change("light.bedroom", function(event)
    if event.new_state.state == "on" then
        ha.after("30m", function()
            ha.call_service("light", "turn_off", { entity_id = "light.bedroom" })
        end)
    end
end)
```

### Scheduler architecture

**New package:** `internal/scheduler/scheduler.go`

```
Scheduler goroutine owns:
  - SQLite rows for all timers
  - min-heap (container/heap) keyed by next_run
  - time.Timer pointed at the soonest next_run
  - onFire func(scriptID, timerID string) callback → routes to script channel

On Start(ctx):
  1. Load all timer rows from SQLite
  2. For each: if next_run <= now → fire immediately (catch-up), update next_run
  3. Build heap, arm timer for soonest next_run
  4. Loop: on timer expiry → fire, update last_run + next_run in SQLite, re-heap

Registration (called from Lua via ha.every / ha.at):
  - Compute stable ID
  - INSERT OR IGNORE into timers (preserves existing last_run/next_run)
  - Add to in-memory heap if next_run not already there
  - Return timerID to script goroutine for local callback map

Script goroutine receives TimerFiredEvent{TimerID} on its event channel.
It looks up timerID in its local map and calls the registered *lua.LFunction.
```

The `onFire` callback is provided by the runner — it sends a `TimerFiredEvent` to the correct script's buffered channel. This keeps the scheduler decoupled from the Lua layer.

### `next_run` computation

- `ha.every("5m")`: `next_run = last_run + 5m` (or `now + 5m` if `last_run` is NULL)
- `ha.at("07:00")`: `next_run = today at 07:00` if that's in the future, else `tomorrow at 07:00`
- `ha.after("30m")`: `next_run = now + 30m`; timer ID is a UUID (multiple concurrent calls are valid)
- Catch-up check for `every`/`at`: `if next_run <= now → fire once, recompute next_run from now`

### `ha.after` persistence semantics

`ha.after` does **not** survive process restarts. The timer ID is a UUID generated at call time; on restart the script generates a new UUID, so there is no way to match the old row to the new callback. On startup, every `"after"` row found in the `timers` table is treated as orphaned: log a warning and delete it.

The row is still written to SQLite before the timer fires so that an unexpected crash mid-interval produces a visible log entry rather than silent loss.

- `ha.every` and `ha.at` always survive restart (stable IDs, catch-up on startup).
- `ha.after` never survives restart — design automations accordingly.

---

## Home Assistant Add-on

The repository root doubles as the add-on directory (single-addon repo convention). HA discovers the add-on from the `config.yaml` at the root.

### `config.yaml` (add-on manifest)

```yaml
name: "HA Lua"
description: "Lua scripting engine for Home Assistant"
version: "1.0.0"
slug: "ha-lua"
init: false
arch:
  - aarch64
  - amd64
homeassistant_api: true        # provides $SUPERVISOR_TOKEN + ws://supervisor/core/websocket
startup: services
boot: auto
map:
  - addon_config:rw            # /addon_config inside container = /config/ha-lua on host
options:
  log_level: "info"
  state_history:
    retention_days: 2
    purge_interval: "1h"
  debug:
    pprof_addr: ""
schema:
  log_level: "str"
  state_history:
    retention_days: "int"
    purge_interval: "str"
  debug:
    pprof_addr: "str?"
```

Key implications:
- **No user-supplied HA URL or token.** The Supervisor injects `$SUPERVISOR_TOKEN` and the internal proxy endpoint is fixed at `ws://supervisor/core/websocket`.
- **Scripts**: live at `/addon_config/scripts/` inside the container, which maps to `/config/ha-lua/scripts/` on the host — editable via Studio Code Server.
- **Data** (SQLite DB): `/data/ha-lua.db` — the Supervisor provides `/data` as a persistent volume, survives add-on updates.

### `Dockerfile` (multi-stage)

```dockerfile
ARG BUILD_FROM=ghcr.io/home-assistant/base-debian:latest
FROM golang:1.24-bookworm AS builder
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags="-s -w" \
    -o /usr/local/bin/ha-lua ./cmd/ha-lua

FROM ${BUILD_FROM}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /usr/local/bin/ha-lua /usr/local/bin/ha-lua
COPY run.sh /run.sh
RUN chmod a+x /run.sh
CMD ["/run.sh"]
```

### `run.sh`

```bash
#!/usr/bin/with-contenv bashio
set -e

exec /usr/local/bin/ha-lua \
    --ha-url="ws://supervisor/core/websocket" \
    --ha-token="${SUPERVISOR_TOKEN}" \
    --scripts-dir="/addon_config/scripts" \
    --database="/data/ha-lua.db"
# Options are read from /data/options.json (written by the Supervisor)
```

The binary reads `/data/options.json` automatically for all other settings (log level, retention, etc.).

### `build.yaml`

```yaml
build_from:
  aarch64: ghcr.io/home-assistant/base-debian:latest
  amd64: ghcr.io/home-assistant/base-debian:latest
```

### CI/CD (`.github/workflows/release.yml`)

On tag push: use `home-assistant/builder` action to build multi-arch images and push to `ghcr.io/<user>/ha-lua`. The `image` field can then be added to `config.yaml` to pull pre-built images from the registry instead of building locally.

### Local development install

Copy (or symlink) the repo into `/addons/ha-lua/` on the HA host via Samba/SSH, then install from **Settings → Add-ons → Local add-ons**. Studio Code Server can edit scripts in `/config/ha-lua/scripts/` immediately.

## Deployment

The daemon is a single static Go binary (no CGO). It runs inside a Docker container managed by the HA Supervisor. Scripts live under `/addon_config/scripts/` (= `/config/ha-lua/scripts/` on the host), editable via Studio Code Server. The binary watches for changes and hot-reloads without restarting the container.

---

## Testing

### Strategy

- **Real SQLite everywhere** — use `:memory:` DSN for in-package tests; `t.TempDir()` when a file path is needed (e.g. fsnotify). No DB mocks.
- **Table-driven tests** for all encoding, aggregation, and scheduling logic.
- **`internal/testutil/db.go`** — shared helpers: `NewTestDB(t)` (opens `:memory:`, runs migrations), seed functions for `state_history`, `timers`, etc.
- **Lua API tests** — spin up a real `*lua.LState`, register the API, run Lua snippets via `L.DoString()`, assert results from Go. Fast because SQLite is in-memory.
- **Benchmarks on hot paths** — see table below. Baseline committed in `benchmarks/baseline.txt`; `make bench-compare` shows the delta (informational, does not block CI).

### Test files

| Package | File | Key test cases |
|---------|------|----------------|
| `internal/db` | `db_test.go` | Schema migrations run cleanly; idempotent (apply twice = no error) |
| `internal/store` | `kv_test.go` | Get/Set/Delete/GetAll; per-script isolation (script A can't read script B's keys); global namespace; JSON round-trip for all types via `store.state()` proxy |
| `internal/state` | `tracker_test.go` | Upsert overwrites `states`; `state_history` appends; both written atomically; `ha.get_entity_ids` and `ha.get_entities` return correct subsets |
| `internal/purge` | `purge_test.go` | Rows older than `retention_days` are deleted; rows within window are kept; `RunOnce()` test harness |
| `internal/scheduler` | `scheduler_test.go` | `ha.every` fires at interval; catch-up fires once when `next_run` is past; `ha.at` computes correct next daily time; `ha.after` fires once and row is deleted; orphaned `ha.after` rows cleaned up on start; min-heap ordering with many concurrent timers |
| `internal/lua` | `json_test.go` | `luaToJSON`/`jsonToLua` round-trip: `nil`, `bool`, `number` (int and float), `string`, nested table, array table |
| `internal/lua` | `api_store_test.go` | `store.state()` loads defaults on first use; persists on write; reloads values across LState re-creation |
| `internal/lua` | `api_ha_test.go` | `ha.on_exception` handler is called on callback error; exception info table has correct fields; `ha.exceptions.log_file` appends to file; `ha.exceptions.email` calls smtp (mocked); `ha.on_state_change` with `initial=true` delivers synthetic events on load |
| `internal/ha` | `client_test.go` | Auth flow against mock WS server (`net/http/httptest`); reconnect loop fires after connection drop; `get_states` seed populates `states` table |

### Benchmarks

| Name | File | What it measures |
|------|------|-----------------|
| `BenchmarkStateInsert` | `internal/state/tracker_test.go` | Throughput of inserting a `state_changed` event (atomic tx: upsert `states` + append `state_history`) |
| `BenchmarkPurge` | `internal/purge/purge_test.go` | Time to delete 10 000 expired rows in one tick |
| `BenchmarkKVGet` | `internal/store/kv_test.go` | Single `store.Get` call |
| `BenchmarkKVSet` | `internal/store/kv_test.go` | Single `store.Set` call (upsert) |
| `BenchmarkStoreStateProxyWrite` | `internal/lua/api_store_test.go` | Single write through the `store.state()` proxy (Lua → JSON → SQLite) |
| `BenchmarkLuaJSONEncode` | `internal/lua/json_test.go` | `luaToJSON` on a realistic HA attributes table (~15 keys) |
| `BenchmarkLuaJSONDecode` | `internal/lua/json_test.go` | `jsonToLua` on the same payload |
| `BenchmarkEventDispatch` | `internal/lua/api_store_test.go` | Dispatch one `state_changed` event to 10 concurrently running scripts, each with a registered callback |
| `BenchmarkSchedulerFire` | `internal/scheduler/scheduler_test.go` | Fire 100 timers from a loaded heap (measures heap-pop + SQLite update per fire) |

### Regression detection

`make bench-compare` runs `benchstat baseline.txt current.txt` and prints the delta table. The output is informational — it does not fail the build. Regressions are caught by eyeballing the output or by reviewing the diff when `bench-update` is proposed. Running `make bench-update` after verifying the numbers creates or updates the committed baseline.

### `benchmarks/` layout

```
benchmarks/
├── baseline.txt   # committed; represents accepted performance
└── .gitignore     # current.txt
```

---

## Lua Standard Library

Scripts get a curated environment: the safe subset of Lua 5.1 built-ins, five new Go-backed modules, and a few additions to the existing `math` table. Dangerous built-ins are nil'd out at LState creation time.

### Sandboxing (applied to every new LState)

Create with `lua.NewState(lua.Options{SkipOpenLibs: true})`, then selectively open:

| Opened | Removed after open |
|--------|--------------------|
| `base` (print, pairs, ipairs, type, tostring, tonumber, error, assert, pcall, xpcall, select, unpack, next, rawget, rawset, rawequal, setmetatable, getmetatable, ipairs) | `load`, `loadstring`, `dofile`, `loadfile`, `require` — removed from `_G` after open |
| `math` | — (augmented, see below) |
| `string` | — |
| `table` | — |
| `os` | `execute`, `exit`, `remove`, `rename`, `setlocale`, `tmpname`, `getenv` — nil'd after open |
| `coroutine` | — |

`io`, `debug`, `package`, `channel` are never opened.

---

### New modules

#### `strings` — backed by `strings` package

Supplements the built-in `string` (singular) with functions Go does natively but Lua requires verbose patterns for:

| Function | Description |
|----------|-------------|
| `strings.contains(s, substr)` | `true` if `substr` is anywhere in `s` |
| `strings.has_prefix(s, prefix)` | prefix test |
| `strings.has_suffix(s, suffix)` | suffix test |
| `strings.split(s, sep)` | returns Lua array table; `sep=""` returns utf-8 characters |
| `strings.join(parts, sep)` | inverse of split; `parts` is a Lua array table |
| `strings.trim_space(s)` | strips leading/trailing whitespace |
| `strings.trim(s, cutset)` | strips any chars in `cutset` from both ends |
| `strings.replace_all(s, old, new)` | plain-string replacement (no pattern magic) |
| `strings.count(s, substr)` | number of non-overlapping occurrences |
| `strings.fields(s)` | splits on whitespace runs, returns array table |
| `strings.to_upper(s)` | Unicode-aware (unlike `string.upper`) |
| `strings.to_lower(s)` | Unicode-aware |

#### `time` — backed by `time` package

| Symbol | Description |
|--------|-------------|
| `time.now()` | returns a time userdata for the current instant |
| `time.parse(layout, value)` | parses `value` using Go layout string; returns time userdata or `nil, err` |
| `time.unix(sec)` | returns time userdata from Unix timestamp |
| `time.parse_duration(s)` | parses `"5m30s"` etc; returns seconds as float |
| `time.RFC3339` | `"2006-01-02T15:04:05Z07:00"` — constant for HA timestamps |
| `time.second` | `1.0` |
| `time.minute` | `60.0` |
| `time.hour` | `3600.0` |
| `time.day` | `86400.0` |

Time userdata methods (accessed via `:`):

| Method | Returns |
|--------|---------|
| `:format(layout)` | string |
| `:unix()` | float64 (seconds since epoch) |
| `:add(seconds)` | new time userdata |
| `:sub(other)` | float64 (seconds between the two times) |
| `:before(other)` | bool |
| `:after(other)` | bool |
| `:year()`, `:month()`, `:day()` | number |
| `:hour()`, `:minute()`, `:second()` | number |
| `:weekday()` | number 0–6 (Sunday = 0) |
| `:is_zero()` | bool |

```lua
local t = time.parse(time.RFC3339, event.new_state.last_changed)
local elapsed = time.now():sub(t)   -- seconds since last state change
if elapsed > 30 * time.minute then
    ha.call_service("light", "turn_off", { entity_id = "light.bedroom" })
end
```

#### `json` — backed by `github.com/go-json-experiment/json` (json/v2)

All JSON work in the project — `store.state()` proxy, attribute parsing, and this Lua module — uses `github.com/go-json-experiment/json` throughout. Key v2 properties relied on:

- **Deterministic map key order** — object keys sorted by default, making encoded output stable across runs (useful for hashing payloads).
- **Strict UTF-8 and no duplicate keys** — fails fast on malformed input rather than silently ignoring it.
- `json.Marshal(v, opts...)` / `json.Unmarshal(data, &v, opts...)` — same shape as v1, with variadic options appended.

When `encoding/json/v2` is accepted into the Go standard library the import path changes; no logic changes are required.

| Function | Description |
|----------|-------------|
| `json.encode(value)` | any Lua value → JSON string; raises Lua error on failure |
| `json.decode(s)` | JSON string → Lua value (`table`/`number`/`bool`/`string`/`nil`) |

```lua
local payload = json.encode({ brightness = 200, color_temp = 4000 })
local data    = json.decode(res.body)
```

#### `re` — backed by `regexp` package with per-LState compiled-regex cache

Compile cost is paid once per unique pattern per LState. The cache is bounded at **256 entries** per LState; when full, the least-recently-used pattern is evicted. Scripts with dynamic patterns (patterns built from runtime values) still benefit from the cache for repeated patterns, and worst-case recompile cost is bounded. Patterns use Go RE2 syntax.

| Function | Description |
|----------|-------------|
| `re.match(pattern, s)` | bool — true if `s` matches `pattern` |
| `re.find(pattern, s)` | string or nil — first match |
| `re.find_all(pattern, s)` | array table of all matches |
| `re.replace(pattern, s, repl)` | all matches replaced; `repl` may use `$1` group references |
| `re.split(pattern, s)` | array table of parts |

```lua
if re.match([[^sensor\.(temperature|humidity)_]], event.data.entity_id) then
    -- only process temp/humidity sensors
end
```

#### `http` — backed by `net/http` package

Uses `L.Context()` (the current callback's context) so HTTP calls respect the per-callback timeout. The default HTTP client timeout is `min(L.Context().Deadline, 10s)`.

| Function | Returns |
|----------|---------|
| `http.get(url, headers?)` | `{status=200, body="...", headers={...}}` or `nil, err` |
| `http.post(url, body, content_type, headers?)` | same |

`headers` is an optional Lua table of `{["X-My-Header"] = "value"}`. Response `headers` is a flat table of the first value per header name.

```lua
local res, err = http.get("https://api.open-meteo.com/v1/forecast?latitude=47&longitude=19&current_weather=true")
if res then
    local data = json.decode(res.body)
    ha.log("info", "temp: " .. data.current_weather.temperature)
end
```

#### `crypto` — backed by `crypto/{md5,sha1,sha256,sha512,hmac,rand}`, `encoding/{hex,base64}`

All inputs and outputs are Lua strings (byte-transparent). Hash outputs are lowercase hex strings by default.

| Function | Returns | Backed by |
|----------|---------|-----------|
| `crypto.md5(s)` | hex string (32 chars) | `crypto/md5` |
| `crypto.sha1(s)` | hex string (40 chars) | `crypto/sha1` |
| `crypto.sha256(s)` | hex string (64 chars) | `crypto/sha256` |
| `crypto.sha512(s)` | hex string (128 chars) | `crypto/sha512` |
| `crypto.hmac_sha256(key, msg)` | hex string | `crypto/hmac` + `crypto/sha256` |
| `crypto.hmac_sha512(key, msg)` | hex string | `crypto/hmac` + `crypto/sha512` |
| `crypto.base64_encode(s)` | standard base64 string | `encoding/base64` StdEncoding |
| `crypto.base64_decode(s)` | string or `nil, err` | `encoding/base64` StdEncoding |
| `crypto.base64url_encode(s)` | URL-safe base64, no padding | `encoding/base64` RawURLEncoding |
| `crypto.base64url_decode(s)` | string or `nil, err` | `encoding/base64` RawURLEncoding |
| `crypto.hex_encode(s)` | hex string | `encoding/hex` |
| `crypto.hex_decode(s)` | string or `nil, err` | `encoding/hex` |
| `crypto.random_bytes(n)` | string of `n` cryptographically random bytes | `crypto/rand` |
| `crypto.random_hex(n)` | hex string of `n` random bytes | `crypto/rand` + `encoding/hex` |
| `crypto.equal(a, b)` | bool (constant-time comparison) | `crypto/subtle` |

`crypto.equal` uses `subtle.ConstantTimeCompare` to prevent timing-based attacks when comparing MACs or tokens.

```lua
-- Verify an incoming webhook signature (common with GitHub, Stripe, etc.)
local expected = "sha256=" .. crypto.hmac_sha256(store.get("webhook_secret"), body)
if not crypto.equal(request_signature, expected) then
    ha.log("warn", "webhook: invalid signature, ignoring")
    return
end

-- Generate a one-time token and stash it
local token = crypto.random_hex(16)   -- 32-char hex = 128 bits of entropy
global.set("pending_token", token)

-- Hash an entity ID for use as a compact, fixed-length key
local key = "cache_" .. crypto.sha256(entity_id):sub(1, 16)
store.set(key, json.encode(attributes))
```

---

### `math` augmentation

Four functions added to the existing `math` table:

| Function | Description |
|----------|-------------|
| `math.round(x)` | round half-away-from-zero |
| `math.clamp(x, min, max)` | clamp `x` to `[min, max]` |
| `math.log2(x)` | log base 2 (missing from Lua 5.1) |
| `math.sign(x)` | `-1`, `0`, or `1` |

---

### Implementation structure

All stdlib registration lives in `internal/lua/stdlib.go` (`RegisterStdlib(L, opts StdlibOpts)`), called during LState setup alongside the HA and store API registration. Each module has its own file:

| File | Module |
|------|--------|
| `internal/lua/stdlib.go` | sandboxing + `RegisterStdlib` entry point |
| `internal/lua/stdlib_time.go` | `time` module + time userdata metatable |
| `internal/lua/stdlib_strings.go` | `strings` module |
| `internal/lua/stdlib_json.go` | `json` module (delegates to existing `json.go` helpers) |
| `internal/lua/stdlib_re.go` | `re` module + per-LState regex cache |
| `internal/lua/stdlib_http.go` | `http` module |
| `internal/lua/stdlib_crypto.go` | `crypto` module |
| `internal/lua/stdlib_math.go` | `math` augmentation |

`StdlibOpts`:
```go
type StdlibOpts struct {
    HTTPTimeout time.Duration  // default 10s; 0 = use context deadline only
}
```

`HTTPTimeout` is read from config (`http_timeout: "10s"`) and passed in at script creation. Add to `config.dev.yaml` and the add-on `options` schema.

---

### Testing

Add to `internal/lua/stdlib_test.go`:

- **Sandboxing**: assert `require`, `loadfile`, `dofile`, `load`, `os.execute`, `os.exit`, `io` are nil/error in a fresh LState.
- **`strings`**: table-driven tests for all 12 functions; edge cases for empty string, UTF-8 input, `split` with empty separator.
- **`time`**: parse RFC3339 → format back → round-trip identical; `:sub` between two times; `:weekday` on known dates; `parse_duration` for all unit letters.
- **`json`**: already covered by `json_test.go` — add `json.encode`/`json.decode` Lua-surface tests.
- **exception handlers**: `ha.exceptions.log_file` — write known error, assert file contents match template; `ha.exceptions.email` — use `net/smtp` test server (or capture SMTP conversation) to verify subject, body fields.
- **`re`**: match / no-match, `find_all` returns correct count, `replace` with group references, cached vs. first-compile performance.
- **`http`**: use `net/http/httptest` test server; assert status, body, headers round-trip; assert context cancellation propagates (cancel context mid-flight, expect `nil, err`).
- **`crypto`**: table-driven against RFC/NIST test vectors for md5, sha1, sha256, sha512, hmac_sha256, hmac_sha512; base64 round-trip (standard + URL-safe); hex round-trip; `random_bytes(n)` produces exactly `n` bytes and two calls differ; `crypto.equal` returns true for equal inputs, false for differing inputs.
- **`math` additions**: `round` at .5 boundary, `clamp` below/above/within, `log2` against known values, `sign` for negative/zero/positive.

Add benchmarks: `BenchmarkReMatchCached` (warm cache) vs `BenchmarkReMatchCold` (first compile), `BenchmarkHTTPGet` against httptest server (measures overhead not network), `BenchmarkTimeNow`, `BenchmarkCryptoSHA256` (hash a 1 KB payload).

---

## Implementation Milestones

1. **HA client** — auth flow, reconnect, raw event stream to stdout
2. **State tracker** — SQLite schema, seed from `get_states`, upsert on `state_changed`
3. **Lua runner** — single script, `ha.log` + `ha.get_state` + `store.*` + `global.*` + `store.state()` proxy working
4. **Event dispatch** — per-script goroutines, `ha.on_state_change` routing, glob matching
5. **Service calls** — `ha.call_service`, `ha.fire_event`
6. **Hot reload** — fsnotify watcher, graceful script restart
7. **History & purge** — `ha.get_history`, `ha.get_entities`, `ha.get_entity_ids`, simple retention-delete purge job
8. **Scheduling** — `ha.every`, `ha.at`, `ha.after` (one-shot), SQLite-backed timer persistence, catch-up on startup, orphaned `ha.after` cleanup
9. **Error handling** — `ha.on_exception`, `ha.exceptions.email` (`net/smtp`), `ha.exceptions.log_file`; per-callback `pcall` + slog fallback
10. **Lua stdlib** — sandboxing + restricted `require`; `strings`, `time`, `json` (v2), `re`, `http`, `crypto` modules; `math` augmentation; `opts.initial` for `ha.on_state_change`
11. **Testing & benchmarks** — full test suite per package, all benchmarks passing, baseline committed
12. **Add-on packaging** — `config.yaml` manifest, `Dockerfile`, `run.sh`, `build.yaml`, `DOCS.md`, `CHANGELOG.md`, GitHub Actions CI/CD
