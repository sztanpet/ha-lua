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
3. Start the add-on. On first start it creates the scripts directory and drops
   a set of read-only examples in `/config/ha-lua/examples/` to copy from.

No URL or token configuration is required: the add-on talks to Home Assistant
through the Supervisor, which provides the connection and an access token
automatically.

## Where things live

| Path                    | What it is                                |
|-------------------------|-------------------------------------------|
| `/config/ha-lua/scripts/`     | Your `*.lua` scripts                |
| `/config/ha-lua/scripts/lib/` | Shared modules loaded with `require`|
| `/config/ha-lua/examples/`    | Bundled reference examples, refreshed every boot — **read-only**, copy into `scripts/` to use |
| `/config/ha-lua/logs/`        | Daemon log (`ha-lua.log`) + script error logs |
| `/data/ha-lua.db`             | persistent add-on data (survives updates) |

The add-on mounts your Home Assistant **config directory** (the one the File
Editor and Samba show as `config`) at `/config`, so the scripts folder is the
same path inside the container and on the host — `config/ha-lua/scripts/`,
right next to your `configuration.yaml`.

Drop `*.lua` files into the scripts directory. Edit them with the **Studio
Code Server** add-on — saved changes reload automatically. Shared helper
modules go under `scripts/lib/` and are loaded with `require`. A script may
also have companion files next to it (e.g. an `.html` page read with
`fs.read`) — copy those alongside the `.lua`.

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
-- The path is relative to /config/ha-lua/logs/.
ha.on_exception(ha.exceptions.log_file("hallway-errors.log"))
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

The daemon writes its log to the **Log** tab in the add-on UI *and* to
`/config/ha-lua/logs/ha-lua.log`, so it survives restarts and you can open it
in the File Editor or Studio Code. Script error handlers registered with
`ha.exceptions.log_file(path)` write to the same directory: the path is
**relative to `/config/ha-lua/logs/`** (subdirectories are fine), so all
logs stay together and a script can never write outside it.

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
| `ha.set_state(entity_id, state, attrs)` / `ha.remove_state(entity_id)` | Publish or remove an entity through the core REST API (non-raising: returns `value\|nil, err`) |
| `ha.on_command(handler)` | Receive `ha_lua_command` events addressed to this script as `handler(action, data)` — the transport the cards use |
| `ha.every(spec, fn)` / `ha.at(time, fn)` / `ha.after(delay, fn)` | Recurring, daily, and one-shot timers (persisted, with startup catch-up) |
| `ha.serve(method, prefix, fn)` | Serve an HTTP route from a script — see *Web UIs* below |
| `ha.log(level, msg)` | Log through the daemon's logger |
| `ha.on_exception(handler)` | Per-script error handler |
| `ha.exceptions.email(cfg)` / `ha.exceptions.log_file(path)` | Built-in error sinks |
| `store.*` | Per-script persistent key-value store; `store.state(defaults)` is an auto-persisting proxy table |
| `global.*` | Key-value store shared across all scripts |
| `require "mod"` | Load a module from `scripts/lib/` |
| `fs.read(path)` / `fs.exists` / `fs.list` / `fs.stat` | Read files in the scripts directory — see *Reading and writing files* below |
| `fs.write(path, content)` / `fs.append` / `fs.mkdir` / `fs.remove` | Write files in the scripts directory |
| stdlib | `strings`, `time`, `json`, `re`, `http`, `crypto`, `fs`; augmented `math` |

For the complete API reference — every function's arguments, return values, and
error behaviour — see [`lua_api.md`](./lua_api.md). For the design rationale, see
`README.md` and `plan.md`.

## Web UIs

A script can serve its own web page and API with `ha.serve`:

```lua
ha.serve("GET", "/api/state", function(req)
  return 200, json.encode({ ok = true }), { ["Content-Type"] = "application/json" }
end)
local PAGE = assert(fs.read("myui.html"))   -- the page lives in its own file
ha.serve("GET", "/", function(req)
  return 200, PAGE, { ["Content-Type"] = "text/html" }
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

## Reading and writing files

The `fs` module gives scripts access to files in the scripts directory —
chiefly so a web UI's HTML/CSS/JS can live in its own file instead of being
embedded as a giant Lua string:

```lua
local html, err = fs.read("dashboard.html")   -- bytes of a sibling file
if not html then ha.log("error", "asset missing: " .. err) end

if fs.exists("overrides.css") then ... end
for _, name in ipairs(fs.list("assets") or {}) do ... end   -- names in a dir
local info = fs.stat("dashboard.html")        -- { size, mtime, is_dir }

fs.mkdir("generated")                         -- mkdir -p semantics
fs.write("generated/report.html", html)       -- create or truncate
fs.append("generated/audit.txt", line)        -- create or append
fs.remove("generated/report.html")            -- file or empty dir
```

- Paths are **relative to the scripts directory** and `/`-separated. A leading
  `/`, `..`, or a symlink pointing outside the directory is rejected — a script
  cannot read or write host files outside its sandbox.
- `fs.read` returns the file contents, or `nil, errmsg` on any error (missing,
  too large, a directory). `fs.exists` returns a boolean and never errors. The
  write functions return `true`, or `nil, errmsg`.
- `fs.write` does not create parent directories (use `fs.mkdir`), and
  `fs.remove` is not recursive.
- Writing a `*.lua` file counts as editing it: the watcher will load or reload
  that script. Everything else is inert to the watcher.
- Files are read **once at load time** in the common case (`local PAGE =
  fs.read(...)`). The hot-reload watcher only watches `.lua` files, so editing
  an asset alone will not reload the script — re-save the `.lua` (or restart the
  add-on) to pick up the change.
- For **data**, prefer `store.*`/`global.*` — they are transactional and
  survive script renames. `fs.write` is for files something else consumes
  (a served page, an export).

## Examples

The add-on ships a set of ready-to-read example scripts. On every start it
writes them into `/config/ha-lua/examples/`, refreshed to the installed
version. That directory is a **read-only reference**: nothing in it is loaded or
run, and your edits there are overwritten on the next start. To use an example,
**copy it into `/config/ha-lua/scripts/`** (helper modules and companion files
included) and edit it there — only `scripts/` is loaded and hot-reloaded.

```sh
cp /config/ha-lua/examples/thermostat.lua   /config/ha-lua/scripts/
cp /config/ha-lua/examples/thermostat.html  /config/ha-lua/scripts/
cp -r /config/ha-lua/examples/lib           /config/ha-lua/scripts/
```

The entity ids in the examples (e.g. in `lib/zones.lua`) are placeholders — edit
them to match your Home Assistant.

## Thermostat example

The flagship example — a heating controller with a web UI — lives in
`/config/ha-lua/examples/`:

| File | Role |
|------|------|
| `thermostat.lua` | Controller + HTTP API. A weekly schedule per zone, timed **overrides** (10/30/60 min + custom) to a per-zone override temperature, and ad-hoc manual holds (when the dial is changed directly). |
| `thermostat.html` | The single-page UI, loaded by `thermostat.lua` via `fs.read`. |
| `heating_windows.lua` | Drops a zone to a frost guard (15 °C) while a window is open and restores the controller's desired setpoint when it closes. |
| `lib/zones.lua` | Shared zone definitions (climate + window entity ids) used by both scripts. **Edit this to match your setup.** |
| `lib/schedule.lua` | Pure schedule math (no I/O). |

To use it, copy all of these from `examples/` into your scripts directory —
**`thermostat.html` must sit next to `thermostat.lua`** (the script reads it with
`fs.read` at load and will error without it) — edit the entity ids in
`lib/zones.lua`, then open
**Heating** from the sidebar (ingress) or add a Webpage card pointing at
`http://<ha-host>:8100/`. Schedules, overrides, and override temperatures are
persisted per zone, so they survive restarts. The controller writes a zone's
setpoint only while its mode is `heat` and no window is open; it never changes
the hvac mode.

## Enhanced climate card

`enhanced_climate.lua` is an alternative to the thermostat example: instead of
defining zones in a file and editing them through an Ingress page, you drop a
**dashboard card** onto a climate entity and configure everything from Home
Assistant. The card provisions the controller, gives a 7-day schedule editor,
timed overrides, and optional window cooperation, and replaces a native `tile`
climate card (current temperature, target, and HVAC mode).

**Install the script** (copy from the read-only examples into your scripts dir):

```sh
cp /config/ha-lua/examples/enhanced_climate.lua  /config/ha-lua/scripts/
cp /config/ha-lua/examples/enhanced_climate.html /config/ha-lua/scripts/
cp -r /config/ha-lua/examples/lib                /config/ha-lua/scripts/
```

**Register the card asset.** The add-on writes the card's JavaScript to
`/config/www/ha-lua/enhanced-climate-card.js` on every start, which Home
Assistant serves at `/local/ha-lua/enhanced-climate-card.js`. Add it once as a
dashboard resource (*Settings → Dashboards → ⋮ → Resources → Add resource*),
URL `/local/ha-lua/enhanced-climate-card.js`, type **JavaScript module**. No
HACS needed.

**Add the card** to a dashboard:

```yaml
type: custom:ha-lua-enhanced-climate-card
climate_entity: climate.living_room           # required — the only must-have
window_sensors: [binary_sensor.living_window] # optional, one or more
radiator_entity: sensor.living_radiator_temp  # optional; display-only, shows
                                              # "rad. X°" on the status line
presets: [10, 30, 60]                         # optional override minutes
name: Living room                             # optional; else friendly_name
```

A GUI editor (the visual card editor, with entity pickers) is also provided, so
the YAML is optional — only the climate entity is required.

**How it works.** The card mirrors two entities: the climate entity itself (for
current temperature, target, and HVAC mode, driven through native climate
services so they keep working even if the daemon is briefly down) and a
**companion sensor** the daemon publishes per climate,
`sensor.ha_lua_enhanced_climate_<slug>` (slug = the climate object id, e.g.
`living_room`), which carries the schedule, boost/override, manual hold, and
window state. The control loop runs in the daemon; the card only edits.

**Removing one.** An enhanced climate is persistent config that outlives the
card — **deleting the card from a dashboard does not remove it.** Remove it from
the **Enhanced climate** Ingress page (the add-on's sidebar panel), which lists
every provisioned climate with a Remove button. This also cleans up climates
left behind by a card you deleted.

Caveats:

- **Admin user required.** The card provisions and edits by firing a
  `ha_lua_command` event through Home Assistant's `events/` REST API, which
  needs an **admin** HA user. Non-admin users still get the climate-native
  controls (target temperature and HVAC mode), which use ordinary service calls.
- **Restart transience.** The companion sensors are published over the REST API
  and are not integration-backed, so a Home Assistant restart drops them; the
  daemon re-publishes them within a minute (and on every reconnect), so they
  self-heal.
- **Recorder.** The companion sensors update at most once a minute and carry
  stable values, but you can keep them out of the recorder by adding
  `sensor.ha_lua_enhanced_climate_*` to your recorder `exclude` if you don't
  need their history.

## Notes

- Scripts are sandboxed: `io`, `os.execute`, `os.exit`, `load`, `dofile`, and
  `package` are unavailable, `require` is restricted to `scripts/lib/`, and the
  `fs` module is read-only and confined to the scripts directory.
- A script that crashes does not affect the others — each runs in its own
  isolated VM.
- Email credentials for `ha.exceptions.email` must come from `store.get(...)`,
  never hardcoded in a script.
