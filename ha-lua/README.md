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

ha.on_exception(ha.exceptions.log_file("/config/hallway-errors.log"))
```

## Lua API

| Function | Purpose |
|----------|---------|
| `ha.on_state_change(pattern, fn, opts)` | Callback on matching entity changes (glob patterns; `opts.initial = true` replays current state on load) |
| `ha.on_event(type, fn)` | Callback on any HA event type (subscribed on demand, even after reload) |
| `ha.get_state(entity_id)` | Current state of one entity |
| `ha.get_entities(pattern)` / `ha.get_entity_ids(pattern)` | Bulk state / ID lookup by glob |
| `ha.get_history(entity_id, since, limit)` | State history from the local mirror |
| `ha.call_service(domain, service, data)` | Call any HA service |
| `ha.fire_event(type, data)` | Fire a custom HA event |
| `ha.serve(method, prefix, fn)` | Serve an HTTP route from a script for a web UI; handler runs on the script's goroutine (pcall'd, 503 on timeout), reachable via an ingress panel and a LAN port |
| `ha.every(spec, fn)` / `ha.at(time, fn)` / `ha.after(delay, fn)` | Recurring, daily, and one-shot timers (SQLite-persisted, fire-once catch-up after restart) |
| `ha.log(level, msg)` | Log through the daemon's logger |
| `ha.on_exception(handler)` | Script-level error handler; gets `{script_id, error, traceback, callback, event, timestamp}` |
| `ha.exceptions.email(cfg)` / `ha.exceptions.log_file(path)` | Built-in exception handlers (email has a per-script cooldown, 15m default) |
| `store.get/set/delete/get_all` | Per-script persistent KV (JSON round-trip, types preserved) |
| `store.state(defaults)` | Proxy table — reads cached, writes auto-persist |
| `global.get/set/delete/get_all` | KV shared across all scripts |
| `require "mod"` | Restricted to `scripts/lib/` only, with module caching and cycle detection |
| `fs.read/exists/list/stat` | Read-only access to files in the scripts dir (`os.Root`-sandboxed against `..`/symlink escapes); e.g. loading a UI's HTML |

Sandboxed stdlib modules are available too: `strings`, `time`, `json`, `re`
(cached regex), `http`, `crypto`, `fs`, plus an augmented `math` (`round`,
`clamp`, `log2`, `sign`). See `plan.md` for the full design.

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
make hooks    # install the pre-commit hook (gofmt + vet + staticcheck + lint)
make build    # compile to ./ha-lua
make check    # vet + staticcheck + lint + race tests — the CI gate
```

From a fresh checkout, `make install` (at the repo root) runs `install-tools`
(pre-fetches the tool sources via `go mod download`) and `hooks` in one step.

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
`/config/scripts/` (that is `/addon_configs/local_ha-lua/scripts/` on the host),
and keeps its database at `/data/ha-lua.db`. The add-on packaging
(`config.yaml`, `Dockerfile`, `run.sh`, `build.yaml`) is the last
milestone and does not exist yet — until then, run the binary standalone
with `--config`.
