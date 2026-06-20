# Changelog

All notable changes to this add-on are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
