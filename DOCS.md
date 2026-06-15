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
| `ha.log(level, msg)` | Log through the daemon's logger |
| `ha.on_exception(handler)` | Per-script error handler |
| `ha.exceptions.email(cfg)` / `ha.exceptions.log_file(path)` | Built-in error sinks |
| `store.*` | Per-script persistent key-value store; `store.state(defaults)` is an auto-persisting proxy table |
| `global.*` | Key-value store shared across all scripts |
| `require "mod"` | Load a module from `scripts/lib/` |
| stdlib | `strings`, `time`, `json`, `re`, `http`, `crypto`; augmented `math` |

For the full design and rationale, see `README.md` and `plan.md` in the
repository.

## Notes

- Scripts are sandboxed: `io`, `os.execute`, `os.exit`, `load`, `dofile`, and
  `package` are unavailable, and `require` is restricted to `scripts/lib/`.
- A script that crashes does not affect the others — each runs in its own
  isolated VM.
- Email credentials for `ha.exceptions.email` must come from `store.get(...)`,
  never hardcoded in a script.
