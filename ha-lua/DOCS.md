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

> **Feels slower than a built-in HA automation?** By default handlers run up to
> 100 ms after an event arrives: events are batched because unbatched bursts
> used to overflow script queues and drop events outright. For automations
> where a human is waiting — a light following a wall switch — put
> `ha.immediate_events()` at the top of the script. See the
> `ha.immediate_events()` section in [`lua_api.md`](./lua_api.md).

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
card can point at `http://<ha-host>:8100/s/<script>/` to embed one script's UI
in a dashboard, or at `http://<ha-host>:8100/` for the tab bar. **This port is unauthenticated** — anyone who can reach it can use
whatever the script exposes. Keep it on the LAN, off the WAN. Set to `0` to
disable the LAN listener (the authenticated ingress panel still works).
Default `8100`.

### Option: `state_history.retention_days`

How many days of entity history to keep. Older rows are deleted by the purge
job. Default `2`.

### Option: `state_history.purge_interval`

How often the purge job runs, as a Go duration (`30m`, `1h`, `6h`). A purge
also runs once at startup. Default `1h`.

### Option: `state_history.keep`

Per-entity retention overrides: a list of `pattern` (an entity glob) and `days`.
The short default keeps the database small, which is right for the two thousand
entities a house reports and wrong for the handful a script asks
`ha.duration_in_state` or `ha.count_changes` about — those need a window long
enough to answer over.

```yaml
state_history:
  retention_days: 2
  keep:
    - pattern: "binary_sensor.*_door"
      days: 30
    - pattern: "climate.*"
      days: 30
```

**First match wins**, so put the narrow patterns first: with `sensor.boiler_*`
before `sensor.*`, the boiler keeps its longer window. `days` may also be
*shorter* than the default, to hold down one chatty entity. Default: empty (the
default retention applies to everything).

### Option: `debug.pprof_addr`

`host:port` to expose Go `net/http/pprof` and execution-trace endpoints for
profiling (e.g. `0.0.0.0:6060`). Leave empty to disable. Only enable
temporarily — it exposes an unauthenticated debug server.

## Lua API at a glance

| Function | Purpose |
|----------|---------|
| `ha.on_state_change(pattern, fn, opts)` | Run `fn` when matching entities change (glob patterns; `opts.initial = true` replays current state on load) |
| `ha.on_event(type, fn)` | Run `fn` on any Home Assistant event type |
| `ha.immediate_events()` | Opt out of the default 100 ms event batching — use for latency-sensitive automations |
| `ha.get_state(entity_id)` | Current state of one entity |
| `ha.get_entities(pattern)` / `ha.get_entity_ids(pattern)` | Bulk lookup by glob |
| `ha.get_history(entity_id, since, limit)` | History from the local mirror |
| `ha.duration_in_state(entity_id, state, since)` | Seconds spent in a state, plus whether history reaches back that far |
| `ha.count_changes(entity_id, since, state)` | Transitions in a window (into `state` if given), plus the same flag |
| `ha.who_changed(entity_id, at)` | What produced a change: a user, an automation/script/scene, or nobody |
| `ha.call_service(domain, service, data, opts)` | Call any Home Assistant service (`opts.wait = false` to not block the handler on HA's confirmation) |
| `ha.fire_event(type, data)` | Fire a custom event |
| `ha.set_state(entity_id, state, attrs)` / `ha.remove_state(entity_id)` | Publish or remove an entity through the core REST API (non-raising: returns `value\|nil, err`) |
| `ha.on_command(handler)` | Receive `ha_lua_command` events addressed to this script as `handler(action, data)` — the transport the cards use |
| `ha.every(spec, fn)` / `ha.at(time, fn)` / `ha.after(delay, fn)` | Recurring, daily, and one-shot timers (persisted, with startup catch-up) |
| `ha.serve(method, prefix, fn)` | Serve an HTTP route from a script — see *Web UIs* below |
| `ha.ui(title)` | Give this script a tab named `title` in the web UI |
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

A script can serve its own web page and API with `ha.serve`, and become a tab
in the daemon's UI with `ha.ui`:

```lua
ha.ui("My Panel")                           -- this script gets a tab

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

### Where a script's routes live

Every script gets its own path namespace: script `myui`'s routes are served
under **`/s/myui/`**. A script that registers `"/api/state"` answers at
`/s/myui/api/state`, and `req.path` still reads `/api/state` — the mount is
stripped before the handler sees it. Two scripts can therefore both serve
`"/"`, which before v4.0.0 they could not: they shared one flat path space and
load order silently decided the winner.

The daemon itself owns `/`. Opening it gives the **tab bar**: one tab per
script that called `ha.ui(title)`, plus a **Debug** tab. The active page loads
in an iframe, so pages cannot collide — each gets its own JS, CSS and
element-ID space — and the daemon never touches a script's HTML. The selected
tab lives in the URL hash (`/#myui`), so reload and back/forward keep it.

`ha.ui` is opt-in: a script that only serves a machine-facing API stays out of
the tab bar. A script that sets a title but registers no `GET "/"` gets a
warning at load, because its tab would open onto a 404.

### The Debug tab

`/debug/` reports what the daemon is doing right now: version and uptime,
goroutine and heap numbers, the HA connection with its reconnect count and last
error, database size, mirrored entity count and write-queue depth, and a row
per script with its routes, timers, queue depth, dropped events and last
exception. It also tails the log live and can capture a goroutine stack dump on
demand — no `pprof_addr` or restart needed.

The log tail is one panel for everything the daemon and your scripts log. Every
`ha.log(level, msg)` call is tagged with the script it came from, and the
**Source** filter narrows the panel to all scripts at once or to a single one;
**Level** filters independently. `print()` is tagged the same way and logs at
info, so debugging output lands here too instead of vanishing into the add-on's
stdout.

### Reaching a UI

A served UI is reachable two ways, both hitting the same routes:

- **Ingress sidebar panel** — authenticated by Home Assistant, shown in the
  left sidebar. Always available; needs no port configuration.
- **Stable LAN port** (`http_port`, default 8100) — for embedding in a
  dashboard with a **Webpage** card. Point it at
  `http://<ha-host>:8100/s/<script>/` for one script's page, or at
  `http://<ha-host>:8100/` for the tab bar. Unauthenticated; see the
  `http_port` option above. Use **relative** fetch URLs (`./api/state`) in your
  page so it works under both entry points.

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

## Battery levels example

`battery_levels.lua` adds a **Batteries** tab listing every battery in the
house: current level, when that level last changed, and an estimate of when it
will hit empty — sorted so the one that dies first is on top.

```sh
cp /config/ha-lua/examples/battery_levels.lua  /config/ha-lua/scripts/
cp /config/ha-lua/examples/battery_levels.html /config/ha-lua/scripts/
```

There is nothing to configure: it picks up `device_class: battery` sensors
(whose state is the percentage) and any entity carrying a numeric
`battery_level` attribute (device trackers, vacuums, some locks).

**Ignoring a battery.** The phone you charge nightly is noise, not a forecast.
Its row's **Ignore** button stops the script sampling it and drops the samples
it had; the row stays on the page — dimmed, always last — with a **Track**
button to change your mind. The choice lives in the script's KV store, so it
survives restarts and reloads.

**How the estimate works.** A rundown forecast needs weeks of history, and the
daemon purges state history after `retention_days` (2 by default), so the script
keeps its own series in its KV store — one sample per observed level change,
which is a handful of rows a month per battery. The drain rate is the **median
of the slopes between every pair of samples**, and the level divided by that
rate gives the ETA. The countdown ends at **15%**, not 0: most hardware goes
flaky well before it stops reporting, and the question here is when to replace a
cell.

A median rather than the obvious drop-between-the-endpoints because real sensors
do not report a clean staircase. Plenty of them swing a point either way with
the daily temperature, and reading the rate off two single samples then made the
answer depend on which side of that swing each one was caught on — the same
battery forecasting nothing, then a month, then a fortnight, twice a day. Pairs
closer together than 12 hours are left out: over half a day a slow drain moves
a fraction of a point, far under what the sensor can report, so those pairs
describe the weather rather than the battery.

Every pair ends at a sample, so the rate only describes steps that have already
finished. The step in progress is handled separately: a level that has held for
a while cannot still be draining faster than one reported step per that dwell,
and that bound is used when it is the smaller of the two. It is loose right
after a step and tightens from there, which is what stops a pack that has sat at
one level for two months from still predicting the rate it drained at before.

A single observed level step is enough, so a coarse 10%-granularity sensor gets
a forecast on its first step rather than after its second. Such an estimate is
hedged on the page — **~4 mo** — and settles as more steps land. Nothing at all
is inferred from a window shorter than 24 hours.

A battery that has not stepped once since tracking started has no rate, but
having held one level for weeks does bound its lifetime. Those rows read
**> 3 mo**: a floor, uncoloured, never a prediction, and always sorted below the
batteries with a real measurement. A row only shows **measuring** when the level
has neither moved nor held long enough to bound.

A rise of 10 points above the run's low point is read as a charge or a battery
swap and restarts the series, so an old slope never leaks into a fresh pack. The
bar is that high because plenty of entities report a level that fluctuates a few
points a day, and it is measured from the low point so that a slow top-up —
which never steps 10 points at once — is caught as well.

Levels are sampled every 15 minutes (and on every page load). The script only
reads — it publishes no entities and sends no notifications.

**When a forecast looks wrong.** Click any row to open its inspector. The
forecast is a pure function of the oldest stored sample and the current level,
so the panel shows exactly that: the window it was measured over, the drop and
the rate, the level at which a rise would wipe the run, every stored sample with
its step and the gap before it, and a trail of the changes behind the number.

That trail is the part worth reading. Each line records what the series did and
what the forecast became — a level stepping, a run wiped by a recharge, the
answer changing kind, or a forecast that moved further than the growing window
explains. Forty lines are kept per battery and they are written as things
happen, not when you go looking, which is the point: a number that swings
between polls is caused by a change to the series that has already overwritten
its own evidence by the time the page is open. A row stuck on **measuring** gets
a plain sentence naming the guard that failed rather than leaving you to guess.

The same thing is available as JSON for a battery you want to watch over time:

```sh
curl -s 'http://homeassistant.local:8100/s/battery_levels/api/detail?entity_id=sensor.attic_battery'
```

Inspecting is read-only — it never samples, so looking at a suspect battery
cannot change it.

## Service API example

`service_api.lua` is one HTTP endpoint that calls **any** Home Assistant
service, for driving Home Assistant from shell scripts, cron jobs, or anything
else that has `curl`.

```sh
cp /config/ha-lua/examples/service_api.lua  /config/ha-lua/scripts/
cp /config/ha-lua/examples/service_api.html /config/ha-lua/scripts/
```

On its first load the script generates an API token and writes it to the add-on
log once (*Settings → Add-ons → HA Lua → Log*, or
`/config/ha-lua/logs/ha-lua.log`) — copy it from there. If you lose it, put your
own in the script's `TOKEN` constant instead.

**The Service API tab** builds the commands for you, token already filled in:
pick a domain, service and entity from your own entities, add whatever fields
the service takes, and copy the finished URL or `curl` line. Each value shows
how the endpoint will read it — `200` as a number, `0123` as text — so a call
is right before you run it, not after. The page only assembles commands; it
never fires one. Note that it carries the token, so anyone who can open it on
the LAN port has it.

```sh
TOKEN=…
API=http://homeassistant.local:8100/s/service_api

# service in the path, data as query parameters
curl -H "X-Auth-Token: $TOKEN" \
  "$API/call/light/turn_on?entity_id=light.kitchen&brightness=200"

# form body — what curl -d sends by default
curl -H "X-Auth-Token: $TOKEN" -d 'entity_id=switch.pump' \
  "$API/call/switch/turn_off"

# JSON body, service named inline
curl -H "X-Auth-Token: $TOKEN" \
  -d '{"service":"notify.mobile_app_phone","message":"backup done"}' \
  "$API/call"
```

Every field that is not `token`, `wait`, `domain` or `service` is forwarded to
Home Assistant as service data verbatim, so the endpoint needs no changes to
call a service it has never heard of.

**Types.** Query and form values are text, so the obvious types are
reconstructed: `true`/`false` become booleans, a value starting with `[` or `{`
is parsed as JSON (`rgb_color=[255,0,0]`), and a number becomes a number only
when the text round-trips exactly — `code=0123` stays the string an alarm panel
expects. `entity_id` may be a comma-separated list. Use a JSON body when you
want exact control; on a collision the body wins over the query.

**Replies** are always JSON — `{"ok":true,…}` with 200, or `{"ok":false,
"error":"…"}` with 400 (malformed request), 401 (missing or wrong token) or 502
(Home Assistant refused the call). The call waits for Home Assistant's verdict
by default, so a 200 means the service actually ran; add `wait=false` for
fire-and-forget. `GET /s/service_api/ping` checks the token without switching
anything on, and `GET /s/service_api/entities` lists your entity ids (both need
the token too).

The port above (`http_port`, 8100 by default) is the LAN port, which has **no
Home Assistant login in front of it** — that is what the token is for, and
since the builder page hands the token to whoever opens it, the token stops
someone who guesses the URL rather than someone on your network. It is plain
HTTP: fine for a script on your own network, not something to port-forward.

## Durable reminders example

"Tell me if the door is still open in ten minutes" is the automation that
breaks in HA: an automation parked in a `delay:` is killed by a restart, so the
reminder disappears exactly when the door has been open the whole time the box
was rebooting. Even our own `ha.after` only survives a restart when it is
registered at load time, and this one is armed from inside a handler.

| File | Role |
|------|------|
| `lib/reminders.lua` | Deferred work kept in the script's store and driven by one load-time tick, so a restart loses nothing. Named actions, escalation ladders, and a throttle that also survives a restart. |
| `door_reminders.lua` | Uses it end to end: nag at 10 min, then 20 min, then an hour, stop when the door closes, and say how long it has really been open (via `ha.duration_in_state`). |

```lua
local reminders = require "reminders"

reminders.define("door_open", function(payload)
  ha.call_service("notify", "persistent_notification", { message = payload.message })
end)

ha.on_state_change("binary_sensor.front_door", function(data)
  if data.new_state.state == "on" then
    reminders.schedule("front_door", "door_open", "10m", { message = "Front door is open" })
  else
    reminders.cancel("front_door")
  end
end)

reminders.start()   -- last, after every define()
```

Actions are named rather than passed as functions because a closure cannot be
stored in SQLite; `define()` them at load time and `schedule()` them by name
from anywhere. Keys are the unit of cancellation — scheduling the same key
twice replaces the pending reminder instead of stacking a second one.

`reminders.throttle(key, window)` is the "do not spam me" gate, returning true
at most once per window, and `reminders.escalate(key, action, steps, payload)`
walks a ladder of delays until something cancels it.

## Notes

- Scripts are sandboxed: `io`, `os.execute`, `os.exit`, `load`, `dofile`, and
  `package` are unavailable, `require` is restricted to `scripts/lib/`, and the
  `fs` module is read-only and confined to the scripts directory.
- A script that crashes does not affect the others — each runs in its own
  isolated VM.
- Email credentials for `ha.exceptions.email` must come from `store.get(...)`,
  never hardcoded in a script.
