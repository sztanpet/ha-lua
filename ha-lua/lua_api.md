# ha-lua Lua API reference

This documents **every** function, module, and value exposed to scripts. For
the add-on's installation and configuration, see [`DOCS.md`](./DOCS.md); for the
architecture and design rationale, see [`README.md`](./README.md).

A *script* is a single `*.lua` file in the scripts directory. Each runs in its
own isolated Lua VM on its own goroutine — scripts never share state except
through `global.*` and the SQLite-backed stores. The script id is the filename
without its extension (`lights.lua` → `lights`).

## Contents

- [Conventions](#conventions)
- [`ha` — core](#ha--core)
  - [Entity state](#entity-state)
  - [Services and events](#services-and-events)
  - [Callbacks (load-time registration)](#callbacks-load-time-registration)
  - [Timers](#timers)
  - [HTTP serving — `ha.serve`](#http-serving--haserve)
  - [Logging](#logging)
  - [Exception handling](#exception-handling)
- [`store` — per-script storage](#store--per-script-storage)
- [`global` — shared storage](#global--shared-storage)
- [`require` — shared modules](#require--shared-modules)
- [Standard library](#standard-library)
  - [`strings`](#strings)
  - [`time`](#time)
  - [`json`](#json)
  - [`re`](#re)
  - [`http`](#http)
  - [`crypto`](#crypto)
  - [`fs`](#fs)
  - [`math` (augmented)](#math-augmented)
- [Sandbox](#sandbox)

## Conventions

**Error reporting comes in two flavours.** Most `ha.*`, `store.*`, and
`global.*` calls **raise** a Lua error on failure (catch with `pcall`); an
uncaught error is routed to the script's [exception handler](#exception-handling).
The I/O-style modules (`http`, `fs`, `json`, parts of `crypto` and `time`)
instead follow the `value | nil, errmsg` convention — they return `nil` plus an
error string as a second value, so you check the first return:

```lua
local body, err = fs.read("page.html")
if not body then ha.log("error", "read failed: " .. err) end
```

Each entry below says which convention it uses.

**Types round-trip through JSON.** Everything stored (`store`, `global`,
`store.state`) and everything crossing the HA boundary (service-call data, event
payloads, entity attributes) is encoded with json/v2. Lua `nil`, `boolean`,
`number`, `string`, and `table` are preserved. A Lua table with sequential
integer keys `1..n` encodes as a JSON array; any other table encodes as a JSON
object. An **empty** table encodes as `{}` (object), so a value you expect to be
a list can arrive as `{}` — guard with a length/`Array.isArray` check on the
consuming side.

**Load time vs. run time.** A script's top-level code runs once at load. The
registration functions — `ha.on_state_change`, `ha.on_event`, `ha.serve`,
`ha.on_exception` — are meant to be called there. Callbacks and timer functions
then fire later, on the same goroutine, so any `ha.*` / `store.*` call inside
them is safe without locking. Timers (`ha.every` / `ha.at` / `ha.after`) may be
registered at load time or from within callbacks.

## `ha` — core

### Entity state

#### `ha.get_state(entity_id)`

Returns the current state of one entity as a table, or `nil` if the entity is
unknown. Raises on a database error.

```lua
local s = ha.get_state("light.kitchen")
-- s = {
--   entity_id    = "light.kitchen",
--   state        = "on",
--   attributes   = { brightness = 200, ... },  -- decoded from JSON
--   last_changed = "2026-06-21T08:00:00+00:00",
--   last_updated = "2026-06-21T08:00:00+00:00",
-- }
```

#### `ha.get_entities(pattern)`

Returns an array of state tables (same shape as `ha.get_state`) for every entity
whose id matches the glob `pattern` (e.g. `"light.*"`, `"sensor.temp_*"`).
Raises on error.

#### `ha.get_entity_ids(pattern)`

Returns an array of entity-id strings matching the glob `pattern`. Cheaper than
`ha.get_entities` when you only need the ids. Raises on error.

#### `ha.get_history(entity_id, since, limit)`

Returns up to `limit` historical state tables for `entity_id` with
`changed_at >= since`, oldest first, read from the local mirror. `since` is a
timestamp string compared lexically against the stored RFC3339 `changed_at`
(e.g. `"2026-06-20T00:00:00+00:00"`). `limit` is an integer. Raises on error.

History depth is bounded by the `state_history.retention_days` option — older
rows are purged.

### Services and events

#### `ha.call_service(domain, service [, data])`

Calls a Home Assistant service. `data` is an optional table of service fields
(including `entity_id`); it is JSON-encoded and sent as-is. Raises on a marshal
or transport error.

```lua
ha.call_service("light", "turn_on", {
  entity_id  = "light.hallway",
  brightness = 200,
})
```

#### `ha.fire_event(type [, data])`

Fires a custom Home Assistant event of the given `type`, with an optional `data`
table (JSON-encoded). Raises on error.

### Callbacks (load-time registration)

#### `ha.on_state_change(pattern, fn [, opts])`

Registers `fn` to run whenever an entity matching the glob `pattern` changes
state. The pattern is validated at load time — a malformed glob raises
immediately rather than silently never firing.

`fn` receives one table:

```lua
ha.on_state_change("binary_sensor.*_motion", function(data)
  -- data.entity_id
  -- data.old_state  -- state table (may be nil on first appearance)
  -- data.new_state  -- state table
end)
```

`opts` is an optional table. `opts.initial = true` replays the entity's current
state to `fn` once at load (so a script that just started acts on the present
state, not only on future changes).

#### `ha.on_event(type, fn)`

Registers `fn` to run on every Home Assistant event of `type`. The daemon
subscribes to the event type on demand and stays subscribed across reloads. `fn`
receives:

```lua
ha.on_event("zha_event", function(ev)
  -- ev.event_type
  -- ev.time_fired   -- timestamp string
  -- ev.data         -- decoded payload table
end)
```

### Timers

All three are persisted in SQLite and survive restarts. On startup the scheduler
fires any missed timer **at most once** (it does not replay every missed
interval) and rebuilds its schedule.

#### `ha.every(spec, fn)`

Runs `fn` repeatedly every `spec`, where `spec` is a Go duration string
(`"30s"`, `"5m"`, `"1h30m"`). Must be positive. Raises on a bad spec.

#### `ha.at(time, fn)`

Runs `fn` daily at `time`, a 24-hour `"HH:MM"` string (e.g. `"07:00"`). The
wall-clock is resolved with the `timezone` option (→ `$TZ` → UTC). Raises on a
bad time.

#### `ha.after(delay, fn)`

Runs `fn` once after `delay` (a Go duration string). Best-effort across
restarts: an `after` timer only survives a restart if it was registered at load
time; one registered from inside a callback is dropped (and logged) if the
process restarts before it fires.

> Timer ids for `every`/`at` are stable across reloads (`script|type|spec|N`),
> so editing a script does not reset their schedule. `after` timers get a fresh
> id per call and their row is deleted once they fire.

### HTTP serving — `ha.serve`

#### `ha.serve(method, prefix, fn)`

Registers an HTTP handler (load-time only) for an exact `method` plus a path
`prefix` (which must start with `/`). Routing across all of a script's handlers
is **exact-method + longest-prefix**; unmatched requests get a 404.

`fn` receives a request table and returns `status[, body[, headers]]`:

```lua
ha.serve("GET", "/api/state", function(req)
  -- req.method   -- "GET"
  -- req.path     -- "/api/state"
  -- req.query    -- { key = "value", ... }  (first value per key)
  -- req.headers  -- { ["Header-Name"] = "value", ... }  (first value per key)
  -- req.body     -- request body string (capped at 1 MiB)
  return 200, json.encode({ ok = true }), { ["Content-Type"] = "application/json" }
end)
```

Return-value handling is defensive: a missing/garbage status defaults to 200, a
missing body to empty, headers are optional. The handler runs on the script's
own goroutine, so `ha.*` / `store.*` are safe inside it — but it must be fast
(SQLite reads, service calls). A handler stuck longer than 5 s makes the client
get a `503`; the handler itself is not aborted. An error in the handler is
routed to `on_exception` and the client gets a `500`.

A served UI is reachable two ways, both hitting the same routes: the
authenticated **ingress** sidebar panel, and the **LAN port** (`http_port`).
Use **relative** fetch URLs (`./api/state`) so a page works under both.

### Logging

#### `ha.log(level, msg)`

Writes `msg` to the daemon log, tagged with the script id. `level` is one of
`"debug"`, `"warn"`, `"error"`; anything else logs at info.

### Exception handling

#### `ha.on_exception(handler)`

Registers a per-script error handler (load-time). Any uncaught error in a
callback, timer, or HTTP handler is passed to `handler` as one table:

```lua
ha.on_exception(function(info)
  -- info.script_id  -- this script's id
  -- info.error      -- the error message
  -- info.traceback  -- the Lua stack trace
  -- info.callback   -- which callback raised (e.g. "on_state_change", "GET /api/x")
  -- info.event      -- the triggering event/state table, or nil
  -- info.timestamp  -- RFC3339 UTC
end)
```

With no handler registered, errors fall back to `slog.Error` in the daemon log.

#### `ha.exceptions.log_file(path)`

Returns a ready-made handler (pass it to `ha.on_exception`) that appends a
formatted record per error to `path`. The parent directory is created if
needed. No rate limiting.

```lua
ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/lights-errors.log"))
```

#### `ha.exceptions.email(cfg)`

Returns a handler that emails each error over SMTP. `cfg` is a table:

| Field | Required | Default | Meaning |
|-------|----------|---------|---------|
| `to` | yes | — | recipient address |
| `smtp_host` | yes | — | SMTP server host |
| `smtp_port` | no | `587` | SMTP server port |
| `username` | no | — | SMTP auth username |
| `password` | no | — | SMTP auth password |
| `from` | no | `username` | sender address |
| `subject_prefix` | no | `[ha-lua]` | subject line prefix |
| `cooldown` | no | `15m` | min interval between sends (Go duration) |

Within the cooldown window further errors are counted, not sent; the suppressed
count and start time are reported in the next email. A failed send also starts
the cooldown (a broken SMTP config will not be retried on every event). Raises
on a bad `cooldown` string or a send error.

> **Never hardcode credentials.** Pull them from `store.get(...)`:
> ```lua
> ha.on_exception(ha.exceptions.email({
>   to = store.get("alert_to"), smtp_host = "smtp.example.com",
>   username = store.get("smtp_user"), password = store.get("smtp_pass"),
> }))
> ```

## `store` — per-script storage

A persistent key-value store scoped to this script. Values round-trip through
JSON, so types are preserved. All functions raise on a database error.

| Function | Returns | Notes |
|----------|---------|-------|
| `store.get(key)` | value or `nil` | `nil` if the key is unset |
| `store.set(key, value)` | — | value may be any JSON-able Lua value |
| `store.delete(key)` | — | |
| `store.get_all()` | table | every key → value for this script |

#### `store.state([defaults])`

Returns a **proxy table**: reads come from an in-memory cache (preloaded from
SQLite, seeded with `defaults`), and every write is mirrored to SQLite
immediately. Convenient for stateful scripts:

```lua
local st = store.state({ count = 0 })
st.count = st.count + 1   -- read cached, write auto-persists
```

Reads of unset keys return `nil`. The cache is per proxy instance (per load).

## `global` — shared storage

A key-value store **shared across all scripts**. Same API and JSON semantics as
`store`, used directly (there is no `global.state()` proxy):

| Function | Returns |
|----------|---------|
| `global.get(key)` | value or `nil` |
| `global.set(key, value)` | — |
| `global.delete(key)` | — |
| `global.get_all()` | table of all global keys |

## `require` — shared modules

`require "name"` loads `scripts/lib/name.lua` and returns its value, caching it
per VM (a module returning nothing is recorded as `true`, per Lua convention).
Circular requires raise a clear error.

`require` is **restricted to `scripts/lib/`**: an absolute path, a `..`, or a
symlink pointing outside the directory is rejected (the latter at the syscall
layer via `os.Root`). Any other module path raises an error.

```lua
local zones = require "zones"   -- scripts/lib/zones.lua
```

## Standard library

These modules are pre-loaded as globals (no `require` needed). The base Lua
`string`, `table`, `math`, and `coroutine` libraries are available; `os` is
restricted to `clock`, `date`, `difftime`, and `time`.

### `strings`

Go `strings`-style helpers (the stock Lua `string` library is also present).

| Function | Returns |
|----------|---------|
| `strings.contains(s, substr)` | bool |
| `strings.has_prefix(s, prefix)` | bool |
| `strings.has_suffix(s, suffix)` | bool |
| `strings.split(s, sep)` | array of parts; `sep == ""` splits into runes |
| `strings.join(tbl, sep)` | string |
| `strings.trim_space(s)` | string |
| `strings.trim(s, cutset)` | string with any leading/trailing `cutset` chars removed |
| `strings.replace_all(s, old, new)` | string |
| `strings.count(s, substr)` | number |
| `strings.fields(s)` | array split on whitespace runs |
| `strings.to_upper(s)` / `strings.to_lower(s)` | string |

### `time`

`time.now()` and friends return a **time object** (userdata) with methods; the
module also has module-level functions and constants.

**Module functions**

| Function | Returns |
|----------|---------|
| `time.now()` | time object for the current instant |
| `time.unix(sec)` | time object from a Unix timestamp |
| `time.parse(layout, value)` | time object, or `nil, err` |
| `time.parse_duration(s)` | duration in **seconds** (e.g. `"1h30m"` → `5400`); raises on bad input |

`layout` uses Go reference-time formatting (`time.RFC3339` is provided as a
constant).

**Module constants**: `time.RFC3339`, `time.second` (1), `time.minute` (60),
`time.hour` (3600), `time.day` (86400) — durations in seconds.

**Time-object methods** (call with `:`)

| Method | Returns |
|--------|---------|
| `t:format(layout)` | formatted string |
| `t:unix()` | Unix seconds |
| `t:add(seconds)` | new time object shifted by `seconds` |
| `t:sub(other)` | difference `t - other` in seconds |
| `t:before(other)` / `t:after(other)` | bool |
| `t:year()` | number |
| `t:month()` | number, 1–12 |
| `t:day()` | day of month |
| `t:hour()` / `t:minute()` / `t:second()` | number |
| `t:weekday()` | number, 0 = Sunday |
| `t:is_zero()` | bool |
| `tostring(t)` | RFC3339 string |

```lua
local now = time.now()
ha.log("info", now:format(time.RFC3339))
if now:weekday() == 0 then ha.log("info", "Sunday") end
```

### `json`

| Function | Returns |
|----------|---------|
| `json.encode(value)` | JSON string (deterministic key order); raises on error |
| `json.decode(s)` | Lua value; raises on invalid JSON |

Encoding follows the table-to-array/object rule in [Conventions](#conventions).

### `re`

Go [RE2](https://github.com/google/re2/wiki/Syntax) regular expressions.
Compiled patterns are cached per VM (256-entry LRU). All raise on an invalid
pattern.

| Function | Returns |
|----------|---------|
| `re.match(pattern, s)` | bool |
| `re.find(pattern, s)` | first match string, or `nil` |
| `re.find_all(pattern, s)` | array of all matches |
| `re.replace(pattern, s, repl)` | string (`repl` may use `$1` group refs) |
| `re.split(pattern, s)` | array of pieces |

### `http`

Outbound HTTP. Both follow the `nil, err` convention and are bounded by the
callback's 5-second context — keep requests quick.

#### `http.get(url [, headers])`

`headers` is an optional table. Returns a response table or `nil, err`:

```lua
local res, err = http.get("https://api.example.com/x", { Authorization = "Bearer …" })
-- res.status            -- number
-- res.body              -- string
-- res.headers           -- { ["Header-Name"] = "first value", ... }
```

#### `http.post(url, body, content_type [, headers])`

`body` is a string, `content_type` sets the `Content-Type` header, `headers` is
an optional table merged on top. Same return shape as `http.get`.

### `crypto`

Hashing, encoding, and CSRNG helpers. Hash and encode functions return a string;
decoders and nothing else follow `nil, err`.

| Function | Returns |
|----------|---------|
| `crypto.md5(s)` / `crypto.sha1(s)` / `crypto.sha256(s)` / `crypto.sha512(s)` | hex digest string |
| `crypto.hmac_sha256(key, msg)` / `crypto.hmac_sha512(key, msg)` | hex HMAC string |
| `crypto.base64_encode(s)` / `crypto.base64_decode(s)` | std base64; decode → `nil, err` |
| `crypto.base64url_encode(s)` / `crypto.base64url_decode(s)` | raw URL base64; decode → `nil, err` |
| `crypto.hex_encode(s)` / `crypto.hex_decode(s)` | decode → `nil, err` |
| `crypto.random_bytes(n)` | `n` cryptographically-random bytes (string); raises on RNG failure |
| `crypto.random_hex(n)` | hex of `n` random bytes; raises on RNG failure |
| `crypto.equal(a, b)` | bool, **constant-time** string compare |

### `fs`

**Read-only** access to files in the scripts directory, sandboxed by `os.Root`
(a leading `/`, a `..`, or a symlink escaping the directory is rejected at the
syscall layer). Paths are relative to the scripts dir and `/`-separated. Chiefly
used to keep a web UI's HTML/CSS/JS in its own file instead of a giant Lua
string.

| Function | Returns |
|----------|---------|
| `fs.read(path)` | file contents (binary-safe string), or `nil, err`. Errors on a directory or a file over **8 MiB** |
| `fs.exists(path)` | bool; never raises (any error → `false`) |
| `fs.list(path)` | array of entry names (non-recursive, no `.`/`..`), or `nil, err`. `fs.list(".")` lists the root |
| `fs.stat(path)` | `{ size, mtime (unix seconds), is_dir }`, or `nil, err` |

```lua
local html = assert(fs.read("dashboard.html"))   -- read once at load
```

> **Hot-reload caveat:** the file watcher only watches `.lua` files. Editing an
> asset (e.g. the HTML) alone will **not** reload the script — re-save the
> `.lua` (or restart) to pick it up.

### `math` (augmented)

The standard Lua `math` library plus:

| Function | Returns |
|----------|---------|
| `math.round(x)` | `x` rounded to the nearest integer |
| `math.clamp(x, min, max)` | `x` constrained to `[min, max]` |
| `math.log2(x)` | base-2 logarithm |
| `math.sign(x)` | `-1`, `0`, or `1` |

## Sandbox

Scripts run with `SkipOpenLibs` and a curated set of libraries. The following
are **removed or unavailable**, by design:

- Globals: `load`, `loadstring`, `loadfile`, `dofile`, `module`, `package`.
- `os` is restricted to `clock`, `date`, `difftime`, `time` — no `os.execute`,
  `os.exit`, `os.getenv`, file ops, etc.
- No `io` library; file access is only the read-only `fs` module.
- `require` resolves only inside `scripts/lib/` (see [`require`](#require--shared-modules)).

A crashing script does not affect the others — each runs in its own VM, and
uncaught errors route to its own [exception handler](#exception-handling).
