# HA Lua

A Lua scripting engine for Home Assistant. It connects to the Home Assistant
WebSocket API, mirrors all entity state into a local SQLite database, and runs
your Lua scripts in response to state changes, events, and timers — with hot
reload, so saving a script reloads it without restarting the add-on.

## Installation

1. Copy this repository into `/addons/ha-lua/` on your Home Assistant host
   (via Samba or SSH), or add it as a custom repository.
2. Go to **Settings → Add-ons → Add-on Store**, find **HA Lua** under
   *Local add-ons*, and install it.
3. Start the add-on. On first start it creates the scripts directory for you.

No URL or token configuration is required: the add-on talks to Home Assistant
through the Supervisor, which provides the connection and an access token
automatically.

## Where things live

| Inside the container        | On the host                       |
|-----------------------------|-----------------------------------|
| `/addon_config/scripts/`    | `/config/ha-lua/scripts/`         |
| `/addon_config/scripts/lib/`| `/config/ha-lua/scripts/lib/`     |
| `/data/ha-lua.db`           | persistent add-on data (survives updates) |

Drop `*.lua` files into the scripts directory. Edit them with the **Studio
Code Server** add-on — saved changes reload automatically. Shared helper
modules go under `scripts/lib/` and are loaded with `require`.

## Your first script

Create `/config/ha-lua/scripts/hallway.lua`:

```lua
ha.on_state_change("binary_sensor.hallway_motion", function(data)
  if data.new_state.state == "on" then
    ha.call_service("light", "turn_on", {
      entity_id = "light.hallway",
      brightness = 200,
    })
    store.set("last_motion", data.new_state.last_changed)
  end
end)

-- Route any error in this script to a log file you can open in Studio Code.
ha.on_exception(ha.exceptions.log_file("/addon_config/hallway-errors.log"))
```

Save it. The add-on log shows the script loading, and the automation is live.

## Configuration

```yaml
log_level: info
timezone: ""
http_port: 8100
state_history:
  retention_days: 2
  purge_interval: "1h"
debug:
  pprof_addr: ""
```

### Option: `log_level`

Daemon log verbosity: `debug`, `info`, `warn`, or `error`. Default `info`.

### Option: `timezone`

IANA timezone name (e.g. `Europe/Budapest`) used to resolve local-time
schedules such as `ha.at("07:00", …)`. Leave empty to fall back to the
container's `$TZ`, then to UTC. State-history timestamps are always stored in
UTC regardless of this setting.

### Option: `http_port`

LAN port for the script-driven web UI (`ha.serve`). A Lovelace **Webpage**
card can point at `http://<ha-host>:8100/` to embed a script's UI in a
dashboard. **This port is unauthenticated** — anyone who can reach it can use
whatever the script exposes. Keep it on the LAN, off the WAN. Set to `0` to
disable the LAN listener (the authenticated ingress panel still works).
Default `8100`.

### Option: `state_history.retention_days`

How many days of entity history to keep. Older rows are deleted by the purge
job. Default `2`.

### Option: `state_history.purge_interval`

How often the purge job runs, as a Go duration (`30m`, `1h`, `6h`). A purge
also runs once at startup. Default `1h`.

### Option: `debug.pprof_addr`

`host:port` to expose Go `net/http/pprof` and execution-trace endpoints for
profiling (e.g. `0.0.0.0:6060`). Leave empty to disable. Only enable
temporarily — it exposes an unauthenticated debug server.

## Lua API at a glance

| Function | Purpose |
|----------|---------|
| `ha.on_state_change(pattern, fn, opts)` | Run `fn` when matching entities change (glob patterns; `opts.initial = true` replays current state on load) |
| `ha.on_event(type, fn)` | Run `fn` on any Home Assistant event type |
| `ha.get_state(entity_id)` | Current state of one entity |
| `ha.get_entities(pattern)` / `ha.get_entity_ids(pattern)` | Bulk lookup by glob |
| `ha.get_history(entity_id, since, limit)` | History from the local mirror |
| `ha.call_service(domain, service, data)` | Call any Home Assistant service |
| `ha.fire_event(type, data)` | Fire a custom event |
| `ha.every(spec, fn)` / `ha.at(time, fn)` / `ha.after(delay, fn)` | Recurring, daily, and one-shot timers (persisted, with startup catch-up) |
| `ha.serve(method, prefix, fn)` | Serve an HTTP route from a script — see *Web UIs* below |
| `ha.log(level, msg)` | Log through the daemon's logger |
| `ha.on_exception(handler)` | Per-script error handler |
| `ha.exceptions.email(cfg)` / `ha.exceptions.log_file(path)` | Built-in error sinks |
| `store.*` | Per-script persistent key-value store; `store.state(defaults)` is an auto-persisting proxy table |
| `global.*` | Key-value store shared across all scripts |
| `require "mod"` | Load a module from `scripts/lib/` |
| stdlib | `strings`, `time`, `json`, `re`, `http`, `crypto`; augmented `math` |

For the full design and rationale, see `README.md` and `plan.md` in the
repository.

## Web UIs

A script can serve its own web page and API with `ha.serve`:

```lua
ha.serve("GET", "/api/state", function(req)
  return 200, json.encode({ ok = true }), { ["Content-Type"] = "application/json" }
end)
ha.serve("GET", "/", function(req)
  return 200, "<!doctype html>…", { ["Content-Type"] = "text/html" }
end)
```

The handler receives `req` (`method`, `path`, `query`, `headers`, `body`) and
returns `status[, body[, headers]]`. Routing is exact-method + longest-prefix
match; unmatched requests get a 404. Handlers run on the script's own goroutine
(so any `ha.*` / `store.*` call is safe) and must be fast — keep them to SQLite
reads and service calls.

A served UI is reachable two ways, both hitting the same routes:

- **Ingress sidebar panel** — authenticated by Home Assistant, shown in the
  left sidebar. Always available; needs no port configuration.
- **Stable LAN port** (`http_port`, default 8100) — for embedding in a
  dashboard with a **Webpage** card (`http://<ha-host>:8100/`). Unauthenticated;
  see the `http_port` option above. Use **relative** fetch URLs (`./api/state`)
  in your page so it works under both entry points.

## Thermostat example

The add-on ships a complete worked example — a heating controller with a web UI
— in the scripts directory:

| File | Role |
|------|------|
| `thermostat.lua` | Controller + HTTP API + single-page UI. A weekly schedule per zone, duration **boosts** (10/30/60 min + custom) to a per-zone comfort temperature, and ad-hoc manual overrides. |
| `heating_windows.lua` | Drops a zone to a frost guard (15 °C) while a window is open and restores the controller's desired setpoint when it closes. |
| `lib/zones.lua` | Shared zone definitions (climate + window entity ids) used by both scripts. **Edit this to match your setup.** |
| `lib/schedule.lua` | Pure schedule math (no I/O). |

To use it, edit the entity ids in `lib/zones.lua`, then open **Heating** from
the sidebar (ingress) or add a Webpage card pointing at
`http://<ha-host>:8100/`. Schedules, boosts, and comfort temperatures are
persisted per zone, so they survive restarts. The controller writes a zone's
setpoint only while its mode is `heat` and no window is open; it never changes
the hvac mode.

## Notes

- Scripts are sandboxed: `io`, `os.execute`, `os.exit`, `load`, `dofile`, and
  `package` are unavailable, and `require` is restricted to `scripts/lib/`.
- A script that crashes does not affect the others — each runs in its own
  isolated VM.
- Email credentials for `ha.exceptions.email` must come from `store.get(...)`,
  never hardcoded in a script.
