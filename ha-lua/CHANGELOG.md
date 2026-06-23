# Changelog

All notable changes to this add-on are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## 2.4.0 - 2026-06-23

### Changed
- The example thermostat's card badge no longer shows the raw `heat` hvac
  mode. It now reads "on" when the zone is in heat mode, and "heating" while
  the device is actively calling for heat (the entity's `hvac_action`), so the
  badge reflects what the radiator is doing. `/api/state` now exposes
  `hvac_action`.

## 2.3.0 - 2026-06-23

### Fixed
- `ha.call_service` now waits for Home Assistant's result and raises an error
  (routed to `ha.on_exception`) when HA rejects the call. It was previously
  fire-and-forget, so a rejected service call — such as a `set_temperature`
  above a device's `max_temp` — failed silently with nothing logged.
- The example thermostat's boost set the climate setpoint to the zone's
  comfort temperature, but the comfort stepper accepted values up to 35°
  while many climate entities only accept up to their advertised `max_temp`
  (commonly 30°). Home Assistant silently drops a `set_temperature` above
  `max_temp`, so a high comfort temp made boost appear to do nothing. The
  controller now reads each entity's `min_temp`/`max_temp` and bounds both the
  comfort temperature and the schedule editor's temperatures to that range.

### Added
- The example thermostat UI lets you tap the target temperature between the
  −/+ buttons to type an exact value, instead of only stepping in 0.1°
  increments. Both the stepper and the manual input clamp to the device's
  advertised temperature range.

## 2.2.0 - 2026-06-22

### Added
- The add-on now ships a set of reference example scripts (the thermostat
  controller and its UI, the window and valve-watch helpers, and the shared
  `lib/` modules). On every start they are written, read-only, into
  `/config/ha-lua/examples/`, refreshed to the installed version. The directory
  is reference only — nothing in it is loaded or run; copy an example into
  `/config/ha-lua/scripts/` and edit it there to use it. The entity ids in the
  examples are placeholders.

## 2.1.0 - 2026-06-21

### Changed
- The thermostat comfort-temperature stepper now adjusts in 0.1° steps
  instead of 0.5°, matching the precision the per-zone schedule editor
  already allows. Comfort values previously set on the 0.5° grid keep
  working unchanged.

## 2.0.0 - 2026-06-21

### Changed
- **BREAKING: `ha.get_history(entity_id, since, limit)` now takes `since` as a
  `time` value, not a string.** It used to be an ISO8601 string compared
  *lexically* against the stored `changed_at`, so callers had to hand-format it
  in UTC — forget the timezone and rows were silently dropped. Pass a `time`
  value instead (e.g. `time.now():add(-time.hour)`); its timezone no longer
  matters, the instant is compared. Scripts that passed a string must be
  updated. The bundled `valve_watch.lua` example already is.

## 1.3.0 - 2026-06-20

### Changed
- **The add-on now uses the Home Assistant config directory.** `/config` is
  mapped into the container; scripts live at `/config/ha-lua/scripts/` (next to
  `configuration.yaml`) and the daemon log is mirrored to
  `/config/ha-lua/logs/ha-lua.log`. The SQLite DB stays at `/data/ha-lua.db`.
  Existing installs must move their scripts to the new path.

### Thermostat example (assets only — no daemon change)
- Internationalization: all UI strings go through a locale table, shipping
  English and Hungarian, with an in-page language picker (also selectable via
  `?lang=`, remembered across reloads).
- Schedule editor reworked: each entry picks the days it applies to — Every
  day, Mon–Fri, Sat–Sun, or an individual day (grouped in the dropdown) —
  instead of a fixed per-day list; setpoints accept tenth-of-a-degree values;
  open/close is animated; the edit button toggles the editor.
- Schedule transition times are shown in the viewer's regional 12h/24h clock.
- "Boost" is renamed **"Temporary override"** and its duration buttons and
  target-temperature stepper are grouped on one line inside a labelled
  outline; clearer custom-duration icon. Default override temperature raised
  from 21 °C to 23 °C.

## 1.2.0

### Added
- Read-only **`fs` module** for scripts: `fs.read`, `fs.exists`, `fs.list`, and
  `fs.stat`, confined to the scripts directory by Go's `os.Root` (symlink and
  `..` escapes are rejected at the syscall layer). Lets a web UI's HTML/CSS/JS
  live in its own file instead of an embedded Lua string.

### Changed
- The thermostat example's single-page UI now lives in `thermostat.html` and is
  loaded via `fs.read`, rather than being embedded as a long string in
  `thermostat.lua`. Editing an asset alone does not hot-reload (the watcher
  watches `.lua` files); re-save the `.lua` or restart to pick it up.

### Security
- `require` now resolves modules through the same `os.Root` as the `fs` module.
  The previous lexical path check could be fooled by a symlink under
  `scripts/lib/` pointing outside the scripts tree; `os.Root` rejects such
  escapes at the syscall layer. No change to how scripts call `require`.

## 1.1.0

### Added
- HTTP **server** for script-driven web UIs: `ha.serve(method, prefix, fn)`
  registers a route; requests are marshaled onto the owning script's goroutine
  (never touching its `LState` from the HTTP goroutine), run under `pcall`, and
  time out to 503 rather than hanging. Routes are owned by the script and
  re-registered on hot reload.
- Two entry points onto the same routes: an authenticated **ingress** sidebar
  panel and a stable, unauthenticated **LAN port** (`http_port`, default 8100)
  for embedding in a dashboard Webpage card.
- New `http_port` option, plus the ingress manifest fields.
- **Thermostat example** scripts: `thermostat.lua` (weekly schedule, duration
  boosts, manual-override detection, controller tick, HTTP API, and a
  self-contained single-page UI) cooperating with a rewritten
  `heating_windows.lua` via a shared published setpoint, with shared
  `lib/zones.lua` and pure `lib/schedule.lua`.

## 1.0.0

First release.

### Added
- Home Assistant WebSocket client with authentication, automatic reconnect
  with backoff, and live event subscription.
- SQLite state tracker: every entity's current state is mirrored and its full
  history appended (WAL mode, single-writer + concurrent readers).
- Per-script Lua VMs (gopher-lua), one `LState` per goroutine, fully
  sandboxed (no `io`, `os.execute`, `load`, `package`, unrestricted `require`).
- Lua API: `ha.on_state_change`, `ha.call_service`, `ha.fire_event`,
  `ha.get_state`, `ha.get_history`, `ha.get_entities`, `ha.get_entity_ids`.
- Timers: `ha.every`, `ha.at`, `ha.after`, persisted in SQLite with
  fire-once catch-up on startup. `ha.at` resolves local time via the
  `timezone` option, `$TZ`, then UTC.
- Per-script and global key-value stores (`store.*`, `global.*`) with JSON
  round-trip, plus the auto-persisting `store.state(defaults)` proxy.
- Exception handling: every callback runs under `pcall`; errors route to
  `ha.on_exception` with a real Lua traceback. `ha.exceptions.email`
  (cooldown-throttled) and `ha.exceptions.log_file` sinks.
- Restricted `require` limited to `scripts/lib/`, with per-VM module caching
  and circular-import detection.
- Standard library modules: `strings`, `time`, `json`, `re` (cached regex),
  `http`, `crypto`; `math` augmented with `round`, `clamp`, `log2`, `sign`.
- Hot reload: scripts are watched and reloaded on change without restarting
  the container.
- State history retention purge on a configurable interval.
- Optional pprof/trace HTTP server via the `debug.pprof_addr` option.
