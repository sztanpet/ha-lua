# ha-lua

A Go daemon that connects to the Home Assistant WebSocket API, mirrors all
entity state into SQLite, and runs your automations as Lua scripts — one
isolated Lua VM per script, with persistent storage, full state history,
and hot reload on file save.

Designed to run as a Home Assistant add-on: a single static binary, no CGO,
no external dependencies at runtime.

## Why

HA automations in YAML stop scaling once you need real logic. ha-lua gives
you a real language with real state: scripts react to entity changes and
events, call services, and keep data across restarts — while the daemon
handles the WebSocket session, reconnects, and bookkeeping.

## A script

Scripts are plain Lua files. Drop one into the scripts directory and it
loads; save it and it reloads; delete it and it stops. No registration, no
manifest, no restarts.

```lua
-- hallway.lua
ha.on_state_change("binary_sensor.hallway_motion", function(data)
  if data.new_state.state == "on" then
    ha.call_service("light", "turn_on", {
      entity_id = "light.hallway",
      brightness = 200,
    })
    store.set("last_motion", data.new_state.last_changed)
  end
end)

ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/hallway-errors.log"))
```

## Lua API

Scripts get a small, deliberate surface: a core `ha` module (entity state,
service calls, events, callbacks, timers, HTTP serving, logging, exceptions),
two SQLite-backed key-value stores (`store` per-script, `global` shared),
restricted `require`, and a sandboxed standard library (`strings`, `time`,
`json`, `re`, `http`, `crypto`, `fs`, plus an augmented `math`).

**[`lua_api.md`](./lua_api.md) is the complete reference** — every function,
its arguments, return values, and error behaviour. A taste:

```lua
ha.on_state_change("binary_sensor.*_motion", function(data)
  if data.new_state.state == "on" then
    ha.call_service("light", "turn_on", { entity_id = "light.hallway" })
    store.set("last_motion", data.new_state.last_changed)
  end
end)

ha.every("5m", function() ... end)                 -- persisted timer
ha.serve("GET", "/", function(req) return 200, html end)  -- a web UI

ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/lights.log"))
```

See `plan.md` for the full design rationale behind the API.

## Architecture

```
HA WebSocket ──► client (auth, reconnect, re-seed, subscriptions)
                   │
                   ├──► state tracker ──► SQLite (current mirror + history)
                   │
                   └──► registry ──► per-script channels ──► one Lua VM
                                                             per goroutine
scripts dir ──► fsnotify watcher ──► supervisor (start/stop/reload)
```

The rules that keep this simple:

- **One Lua VM per script, owned by one goroutine.** VMs are never shared,
  so scripts need no locks and a misbehaving script cannot corrupt another.
- **Two SQLite handles per database.** A single-connection write handle
  serializes all writes (no `SQLITE_BUSY`, ever); a pooled read handle lets
  every script query concurrently. WAL mode makes this safe.
- **Reconnects heal everything.** Every reconnect re-authenticates,
  re-seeds the state mirror (deduplicated — no phantom history rows), and
  re-subscribes. Scripts keep running through it.
- **Stops are drains, not kills.** Reloading a script lets queued events
  finish; only a script stuck for 5 seconds gets its VM aborted.

## Design decisions

**Pure Go, no CGO.** SQLite is `modernc.org/sqlite`, Lua is
`github.com/yuin/gopher-lua`. One `CGO_ENABLED=0` binary cross-compiles to
every add-on architecture without a C toolchain.

**Why gopher-lua (Lua 5.1) and not a newer Lua?** We considered
`github.com/arnodel/golua`, which implements Lua 5.5 and has genuinely
attractive features (hard CPU/memory quotas). We stayed on gopher-lua:

- *Maturity.* gopher-lua is 11+ years old, v1.x, and battle-tested across
  the Go ecosystem. golua is v0.x with a single maintainer and no API
  stability promise. A daemon meant to run unattended for months should sit
  on boring foundations.
- *Binding ergonomics.* golua's continuation-passing API makes every Go
  function exposed to Lua longer and more ceremonial than gopher-lua's
  stack-based `func(L *lua.LState) int`. With ~30 bindings, that is a real
  cost paid on every line, for elegance that never surfaces in scripts.
- *Cancellation model.* Script shutdown is built on gopher-lua's
  `SetContext` — cancelling a context aborts a runaway VM mid-callback.
  golua's quota model is a different mechanism and would force a redesign
  of the supervisor's stop semantics.
- *Number semantics.* Lua 5.3+ integers would split the number type
  through the whole JSON round-trip layer (KV store, `store.state`,
  event payloads). More code, subtle behavior changes, no benefit for
  automation scripts.
- *What 5.5 would buy* — integer division, bitwise operators, `goto`,
  `<const>`/`<close>` — is close to invisible in event-handler scripts.

The VM is fully contained inside `internal/lua`; if the trade-offs shift,
swapping it stays a one-package job.

**No vendoring.** A `vendor/` tree for this project measures **241 MB** —
226 MB of it is `modernc.org` (pure-Go SQLite is a machine-translated C
library). `go.sum` already pins every dependency by checksum and the Go
module proxy guarantees availability, so vendoring would buy nothing but a
bloated repository and unreviewable dependency-bump diffs. If a hermetic
offline build is ever needed, `go mod vendor` is one command away.

**JSON via json/v2** (`github.com/go-json-experiment/json`): strict UTF-8
handling, and the import path swaps to `encoding/json/v2` when it lands in
the standard library. Note: deterministic key order is *opt-in*
(`json.Deterministic(true)`) — every marshal site that needs stable output
must say so.

## Development

Requires Go (see `go.mod` for the version). The analyzers used by
`make check` — `staticcheck`, `benchstat`, and `golangci-lint` — are pinned
as `tool` dependencies in `go.mod`, so `go tool` builds them on demand; there
is nothing extra to install.

```sh
git clone https://github.com/sztanpet/ha-lua
cd ha-lua
make install  # one-shot setup for a fresh checkout (run at the repo root)
make build    # compile to ./ha-lua
make check    # vet + staticcheck + lint + race tests — the CI gate
```

From a fresh checkout, `make install` (at the repo root) does the one-time
setup in one step:

- `install-tools` — pre-fetches the analyzer sources via `go mod download`.
- `hooks` — installs the pre-commit hook (gofmt + vet + staticcheck + lint).
- `check-browser` — checks for the Chrome/Chromium binary the browser-driven
  UI tests (`internal/lua`, chromedp) need. Those tests skip cleanly when no
  browser is found, so this only **warns**; it never fails the build. Set
  `CHROMEDP_BROWSER=/path/to/chrome` to point the tests at a specific binary,
  otherwise the usual `google-chrome`/`chromium` names on `$PATH` are used.

Run it outside Home Assistant with a YAML config:

```yaml
# config.dev.yaml
homeassistant:
  url: "ws://homeassistant.local:8123/api/websocket"
  token: "your_long_lived_access_token"
scripts_dir: "./scripts"
database: "./ha-lua.db"
log_level: "debug"
```

```sh
./ha-lua --config config.dev.yaml
```

Other targets:

| Target | What it does |
|--------|-------------|
| `make test` | `go test -race ./...` |
| `make fmt` | gofmt the tree (the pre-commit hook enforces this) |
| `make tidy` | `go mod tidy` |
| `make update-deps` | Upgrade all dependencies (incl. test/tool deps) + tidy; review and `make check` before committing |
| `make bench` / `bench-compare` / `bench-update` | Benchmarks against a committed baseline (informational) |
| `make profile-cpu` / `make trace` | pprof / execution trace (needs `debug.pprof_addr` set) |

## Deployment

The intended deployment is a Home Assistant add-on (repo root = add-on
directory): the Supervisor injects `$SUPERVISOR_TOKEN` and the binary
reads user options from `/data/options.json`, scripts from
`/config/ha-lua/scripts/` (the HA config directory is mounted at `/config`,
so that is `config/ha-lua/scripts/` next to your `configuration.yaml`),
and keeps its database at `/data/ha-lua.db`. The add-on packaging
(`config.yaml`, `Dockerfile`, `run.sh`, `build.yaml`) is the last
milestone and does not exist yet — until then, run the binary standalone
with `--config`.
