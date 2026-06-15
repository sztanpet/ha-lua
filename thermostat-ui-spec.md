# Thermostat UI — Specification (draft)

Status: **ready to build**. All design decisions are resolved (see §12);
everything below reflects the agreed choices.

## 1. Goal

A tiny, purpose-built web UI, served by ha-lua and shown in Home Assistant,
for two jobs:

1. **Set a weekly heating schedule** (changed rarely).
2. **Request heating for a fixed duration** (10 / 30 / 60 min presets + custom),
   after which the zone reverts to the schedule.

## 2. Locked decisions

| Decision | Choice |
|----------|--------|
| Embedding | **Ingress + a stable LAN port** — Ingress gives an authenticated sidebar panel; the fixed `http://<host>:<port>/` URL is what a dashboard Webpage card embeds (and what dev mode uses). Same server, same routes, two entry points. |
| Scope | **Three independent zones**: bedroom, livingroom, childrens-room |
| Schedule model | **Full 7-day**, each weekday has any number of `time → temp` transitions; **setpoint only — never turns a zone off** |
| Boost target | **Per-zone comfort temp**, UI-settable — each zone has its own prominent stepper, not buried in config |

### 2.1 UI effort hierarchy (from your clarification)

Listed least → most prominent. This drives the whole layout:

1. **Schedule editing** — hide as much as possible (rare; tucked behind an
   "edit" affordance).
2. **Today's schedule** — *shown*, read-only: a glanceable timeline of today's
   `time → temp` steps with the current step highlighted.
3. **Boost temperature** — easily settable: a prominent +/- stepper.
4. **Initiating a boost** — *maximum* effort/prominence: big duration buttons
   (10 / 30 / 60 + custom) are the hero of the page.

## 3. The new capability: a Lua-facing HTTP server

ha-lua today has an HTTP *client* only. This feature adds an HTTP *server* that
scripts can drive, so the thermostat UI is itself a ha-lua script (HTML/JS
served by the script; button presses POST back to the script, which calls
`climate.set_temperature` and arms timers). Reusable for any future UI.

### 3.1 Concurrency model (the critical constraint)

Each script owns one `*lua.LState`, used **only** by its own goroutine —
gopher-lua is not goroutine-safe. The HTTP server runs in its own Go
goroutine(s) and therefore **must never touch an LState directly**.

So an incoming request is marshaled onto the target script's goroutine exactly
like timer/event dispatch, but as a **request/response** round-trip:

```
HTTP goroutine                     script goroutine (owns LState)
  parse request
  build RequestEvent{req, replyCh}
  send on script channel  ───────► run loop receives RequestEvent
  block on replyCh                   pcall(lua_handler, req)  → resp
  (with timeout)          ◄───────   replyCh <- resp
  write HTTP response
```

- Handlers run serially through the script's existing event loop — fine for a
  personal dashboard. Handlers must be fast (SQLite reads, service calls); no
  long blocking work.
- The Lua handler runs on the script goroutine, so it may call any `ha.*` /
  `store.*` API normally.
- Each dispatch is wrapped in `pcall` and routed to `ha.on_exception` on error,
  consistent with the rest of the codebase. A handler timeout returns HTTP 503.

**Requests must NOT use the lossy event fan-out.** The event router delivers
state/event messages non-blocking and *drops + warns on full* (see CLAUDE.md) —
acceptable for fire-and-forget events, fatal for a request/response (a dropped
request is a hung dashboard). The request path therefore uses a **dedicated
per-script request channel with a blocking send up to the handler timeout**; on
timeout the HTTP goroutine gives up and returns **503** (it never silently
drops). This is a separate path from `DispatchToTimer` / the event fan-out.

### 3.1a Route lifecycle (hot reload)

Routes are registered at load time via `ha.serve` and are **owned by the
script**, exactly like the per-script event-type subscriptions the supervisor
already manages (M6). The daemon keeps a route table mapping `(method, prefix)`
→ the owning script's request channel:

- **On (re)load**: the supervisor clears the script's old route entries before
  the new load re-registers them, so the table never points at a torn-down
  goroutine.
- **On stop**: the script's routes are removed; subsequent requests to them
  return 404.
- A request that arrives in the gap between stop and re-register returns 503,
  not a hang.

Without this, a reload of the UI script would leave `/` pointing at a dead
goroutine and every request would hang until timeout.

### 3.2 Proposed Lua API

```lua
-- Register a handler for a method+path prefix. req fields:
--   req.method, req.path, req.query (table), req.headers (table), req.body (string)
-- handler returns: status:int, body:string, headers:table (optional)
ha.serve("GET",  "/api/state",  function(req) ... return 200, json.encode(t), {["Content-Type"]="application/json"} end)
ha.serve("POST", "/api/boost",  function(req) ... end)
ha.serve("GET",  "/",           function(req) return 200, HTML, {["Content-Type"]="text/html"} end)
```

- Routing: exact method + longest-prefix path match; unmatched → 404.
- v1 serves a **single self-contained HTML file** (inline vanilla JS/CSS, no
  build step, no external assets) returned by the `GET /` handler. A static
  asset directory is out of scope for v1 (`io` is sandboxed; no file-serving
  API exists yet).

### 3.3 RESOLVED — two entry points: Ingress + stable LAN port

One HTTP server, one set of routes, reachable two ways:

1. **Ingress** (authenticated sidebar panel). `config.yaml` gains
   `ingress: true`, `ingress_port: 8099`, `panel_icon: mdi:thermostat`,
   `panel_title: "Heating"`. The Supervisor proxies it; requests carry
   `X-Ingress-Path`. The UI uses **relative** fetch URLs (`./api/state`) so it
   works under the rotating ingress base path. This is for casual one-tap
   access from the sidebar.
2. **Stable LAN port** (dashboard embedding + dev). `config.yaml` exposes a
   fixed port via `ports:` (e.g. `8100/tcp`); a Lovelace **Webpage card**
   points at `http://<ha-host>:8100/`. This is the URL that actually lives in a
   dashboard view, and it is the port dev mode (`config.dev.yaml`) serves on —
   there is no Supervisor/ingress outside the add-on.

The daemon binds both listeners to the same handler set; relative fetch URLs
keep the page agnostic to which entry point loaded it.

> Note: ingress *can* be embedded in a dashboard view, but only via a
> third-party custom Lovelace card (e.g. `lovelylain/ha-addon-iframe-card`) —
> HA's built-in iframe/Webpage card can't, because the `/api/hassio_ingress/…`
> URL needs a per-session token the stock card can't mint. We deliberately use
> the LAN port for dashboard embedding to avoid that custom-card dependency;
> the ingress sidebar panel remains available for authenticated access.

**Security posture:** the LAN port has **no authentication** — anyone who can
reach `http://<ha-host>:8100/` can boost heating and edit the schedule. This is
an accepted LAN-trust trade-off (your choice) for true in-dashboard embedding.
Mitigations to consider: bind only to the LAN interface, keep it off the WAN,
and treat it as you would any unauthenticated LAN device. The Ingress path
remains fully authenticated.

## 4. Control model — who owns the setpoint

Two scripts cooperate, with a clean split so they never write conflicting
values:

- **`heating_windows.lua`** owns the *window* dimension: a window opening drops
  the zone to the frost guard (15 °C); a window closing restores the
  setpoint — but the value it restores is whatever the thermostat *currently
  wants*, not a stale manual save (§4.2).
- **The thermostat controller** owns the *schedule / boost / manual-override*
  dimension. It computes one desired setpoint per zone, **publishes** it, and
  **writes it to the climate entity only while no window in the zone is open**.

### 4.1 `desired()` — what the thermostat wants

```
desired(zone) =
    comfort(zone)     if a UI boost is active and not expired
    override_temp     elif a manual ad-hoc override is active (until next transition)
    scheduled_temp    else
```

A UI boost outranks a manual override: an explicit duration press is a stronger
signal than a dial nudge, and it keeps behavior predictable (no rubber-banding
ambiguity).

There is **no window branch** here — windows are `heating_windows.lua`'s job.
On every tick and relevant event the controller:

1. **Publishes** `desired(zone)` to `global.set("thermostat:desired:"..zone, t)`
   so the window script knows what to restore.
2. **Writes** it to the climate entity **only if** mode is `heat` **and** no
   window in the zone is open (your instruction: the thermostat must not set
   anything while a window is open).
3. Calls `set_temperature` only when the value actually changes, to avoid
   spamming the service.

### 4.2 RESOLVED — `heating_windows.lua` is kept and made schedule-aware

`heating_windows.lua` is **kept** (its functionality is needed), not retired —
it is taught to cooperate. The two scripts share state through `global.*`:

- `heating_windows.lua`, **window opens** → `set_temperature(zone, frost_temp)`.
- `heating_windows.lua`, **window closes** → read
  `global.get("thermostat:desired:"..zone)` and write that. It restores the
  *current* schedule/boost value, never the stale pre-open setpoint. This is the
  fix for the override-on-restore bug you called out.

The old "save the pre-open setpoint and restore it" logic is removed from
`heating_windows.lua`; it trusts the published desired instead. Because the
controller skips its own write whenever a window is open, and the window script
writes exactly the published desired on close, the two can never fight.

> Alternative: merge both into the controller and delete `heating_windows.lua`.
> Functionally identical; the chosen default keeps the file per your
> instruction. Either way `frost_temp` (config) is the single source for 15 °C.

### 4.3 RESOLVED — setpoint only, never manages mode

The controller **never changes the hvac mode**. A "night" period is just a low
temperature, not an `off`. When a zone's mode is `off` the controller leaves it
entirely alone (shown as "not controlled"). This matches the earlier "only act
when mode is heat" rule and keeps behavior predictable.

## 5. Data models

### 5.1 Zone config (in the script)

```lua
local zones = {
  bedroom    = { climate = "climate.bedroom",        windows = {"binary_sensor.bedroom_window"} },
  livingroom = { climate = "climate.livingroom",     windows = {"binary_sensor.livingroom_window"} },
  childrens  = { climate = "climate.childrens_room",  windows = {"binary_sensor.childrens_room_window"} },
}
```

The thermostat and `heating_windows.lua` **must agree on the zone keys** —
they're the `<zone>` in `global:thermostat:desired:<zone>`. Put this table in a
shared module under `scripts/lib/` and `require` it from both, so the two
scripts can't drift (today `heating_windows.lua` has its own independent rooms
table — that gets replaced by the shared one).

### 5.2 Schedule (persisted per zone in `store`)

Key `schedule:<zone>` → JSON. `days` is index 0=Mon … 6=Sun, each a list of
transitions sorted by time. The active temp is the most recent transition at or
before "now"; before the first transition, the last transition of the previous
day carries over.

```json
{
  "days": {
    "0": [ {"time": "06:30", "temp": 21}, {"time": "08:00", "temp": 18},
           {"time": "17:00", "temp": 21}, {"time": "22:00", "temp": 16} ],
    "...": []
  }
}
```

### 5.3 Boost state (persisted per zone in `store`)

```json
{ "active": true, "ends_at": "2026-06-15T18:30:00Z" }
```

Persisting `ends_at` (absolute UTC) makes restart handling trivial: on startup
the controller drops any boost whose `ends_at` is past and resumes the rest.

### 5.3a Manual override state (persisted per zone in `store`)

Set when a user changes the setpoint directly in HA (§9). Holds until the next
schedule transition time, then cleared.

```json
{ "temp": 22, "until": "2026-06-15T17:00:00Z" }
```

`until` is the absolute UTC time of the next schedule transition at the moment
the override was detected. **Boost wins:** while a UI boost is active, manual
dial changes are ignored (no override is started — the controller holds comfort
on the next tick). Pressing a boost button also clears any existing override.

### 5.4 Settings (persisted per zone in `store`, UI-settable)

The comfort/boost temperature is **per zone** and a live UI control (each zone
gets its own stepper, §2.1), so it lives in `store`, keyed per zone, and is
edited from the page.

```json
// key "comfort:<zone>"
{ "comfort_temp": 21 }
```

### 5.5 Config locations — who can read what

**Important constraint:** Lua scripts have **no API to read `config.yaml` /
options.json** — the Runner is handed only `tracker / scheduler / store /
global` (`supervisor.go` `Deps`); there is no config bridge. So a value belongs
in `config.yaml` *only if the daemon (Go) consumes it*. Anything a script needs
must live as a script constant or in `store` / `global`.

**Daemon-level (`config.yaml`)** — consumed by the Go HTTP-server subsystem:

```yaml
# manifest fields
ingress: true
ingress_port: 8099
panel_icon: mdi:thermostat
panel_title: "Heating"
ports:
  "8100/tcp": 8100   # stable LAN port
# options (also added to the Config struct + schema)
http_port: 8100      # which port the LAN listener binds
```

**Script-level** — not in `config.yaml`:

- `frost_temp` (15 °C) — a constant in `heating_windows.lua`, optionally made
  UI-editable later via `global` (`global:thermostat:frost_temp`).
- `boost_presets` (`{10, 30, 60}`) — a constant in the UI script.
- `default_comfort` (21 °C) — a constant the controller uses to seed a zone's
  `comfort:<zone>` the first time, before the user touches the stepper.

## 6. HTTP API (served by the script)

| Method | Path | Body / Query | Response |
|--------|------|--------------|----------|
| GET | `/` | — | the HTML UI |
| GET | `/api/state` | — | per-zone live status incl. each zone's comfort temp (below) |
| POST | `/api/boost` | `{zone, minutes}` | boost to current comfort temp for N min; returns new state |
| POST | `/api/boost/cancel` | `{zone}` | clear boost, revert to schedule |
| PUT | `/api/settings` | `{zone, comfort_temp}` | update that zone's comfort/boost temp (its stepper) |
| GET | `/api/schedule` | `?zone=` (optional) | one zone's (or all zones') full 7-day schedule, for that card's editor |
| PUT | `/api/schedule` | `{zone, days}` | save + re-apply schedule |

`GET /api/state` — drives the whole page in one poll, including each zone's
comfort stepper value and **today's** schedule (so the page never needs the
full schedule unless the editor is opened):

```json
{
  "zones": {
    "bedroom": {
      "mode": "heat",
      "current_temp": 19.4,
      "target": 21,
      "comfort_temp": 21,
      "window_open": false,
      "scheduled_temp": 18,
      "today": [ {"time":"06:30","temp":21}, {"time":"08:00","temp":18},
                 {"time":"17:00","temp":21}, {"time":"22:00","temp":16} ],
      "now_index": 2,
      "boost": { "active": true, "ends_at": "...", "remaining_s": 1234 }
    }
  }
}
```

The UI polls `/api/state` every ~5 s (simple; no websocket/SSE in v1).

## 7. UI sketch (single page, mobile-first)

```
┌──────────────────────────────────────┐
│  Heating                             │
│                                      │
│  ┌─ Bedroom ──────────── heat 19.4° ┐│
│  │  ███ BOOST  10m   30m   60m   ⌨  ││  ← HERO: big tap targets
│  │  to  [ − ]  21°  [ + ]           ││  ← this zone's comfort stepper
│  │  ⏱ boosting → 18:30  [ cancel ]  ││
│  │  today 06:30 21°│08:00 ▶18°│22:00 16°││  ← read-only strip
│  │                          ✎ schedule ││  ← this zone's edit button
│  └──────────────────────────────────┘│
│  ┌─ Livingroom ───────── heat 21.0° ┐│
│  │  ███ BOOST  10m   30m   60m   ⌨  ││
│  │  to  [ − ]  20°  [ + ]           ││
│  │  today 06:00 20°│22:30 16°        ││
│  │                          ✎ schedule ││
│  └──────────────────────────────────┘│
│  ┌─ Childrens room ─────────── off  ┐│
│  │  (mode off — not controlled)     ││
│  │                          ✎ schedule ││  ← editable even when off
│  └──────────────────────────────────┘│
└──────────────────────────────────────┘
```

Each zone is a fully self-contained card: its own boost buttons, comfort
stepper, today strip, and its own **✎ schedule** button. There is no global
editor — editing is per zone.

Layout follows §2.1, top (rare) to bottom-emphasis (frequent):

- **Hero — boost buttons.** Large, thumb-sized `10 / 30 / 60` + a custom-minutes
  entry, per zone. One tap heats that zone to the current comfort temp for the
  chosen duration. An active boost flips the row to a live countdown + Cancel.
- **Boost temperature stepper.** Each zone card has its own prominent
  `− value +` control (PUT `/api/settings`) — the temperature that zone's boost
  buttons heat to.
- **Today's schedule strip.** Read-only, glanceable; the current step marked
  (`▶`). Comes straight from `state.zones[z].today` / `now_index`.
- **Schedule editor.** Per zone, behind that card's small **✎ schedule**
  button; opened rarely. Editing one zone shows only that zone's 7-day grid
  (`GET /api/schedule?zone=…`, `PUT /api/schedule {zone, days}`), each day a
  list of `time → temp` rows with add/remove.
- Vanilla JS, no framework. Polls `/api/state` (~5 s); optimistic UI on actions.

## 8. Scheduling / enforcement engine

- A single `ha.every("1m", ...)` tick per controller drives everything: for
  each zone recompute `desired()`, **publish** it to
  `global:thermostat:desired:<zone>`, and write it to the climate entity if mode
  is `heat` and no window is open. This is restart-safe and needs no reliance on
  best-effort `ha.after`.
- Boost / override expiry is handled by the same tick (compare `now` vs
  `ends_at` / `until`); the expired row is cleared and the zone reverts to
  schedule on the same pass.
- Event-driven recompute also fires on:
  - **window open/close** — re-publish/recompute promptly (the window script
    reads the freshly published desired on close);
  - **external climate target change** (manual override detection, §9) — while
    the zone's windows are closed, a target ≠ published desired starts an
    override until the next transition.
- **Transition timing.** Schedule transitions are detected by the 1-min tick, so
  the published desired can lag a transition by up to one minute. If a window
  closes in that gap, the window script restores the *pre-transition* value and
  the next tick self-corrects. Acceptable for v1. Precise alternative: register
  an `ha.at` per transition that republishes immediately — defer unless the
  1-min lag proves annoying.

## 9. Edge cases

- **Manual setpoint change by a user** (in HA directly): RESOLVED — treated as
  an **ad-hoc override that holds until the next schedule transition**, then the
  schedule resumes. Detection is clean because the controller is the only thing
  that ever writes `desired`, and it always writes *exactly* `desired`: so a
  climate target that differs from the published `desired` is external (the
  user) → start an override `{temp, until = next transition time}`. An override
  is **not** started when (a) a UI boost is active (boost wins, see §5.3a), or
  (b) a window in the zone is open (that's the window script's 15 °C / restore
  territory). The window script's restore writes exactly `desired`, so it never
  looks like a manual change; the controller's own writes never do either.
  - **Load-bearing dependency:** the "window open?" check uses
    `ha.get_state(window)`, which reads the tracker's mirror. This is correct
    only because the tracker is the *synchronous* state consumer (CLAUDE.md), so
    the mirror already reflects the open state by the time the frost-write echo
    arrives — no false override. A future change to tracker ordering would break
    this; keep them coupled. If `get_state(window)` is `nil` (not yet seeded at
    startup), treat as **unknown → do not start an override**.
- **Window opens during a boost**: `heating_windows.lua` sets 15 °C; the
  controller keeps the boost running and keeps *publishing* comfort but does not
  write (window open). On close the window script restores the published
  desired — comfort if still within the boost window, else the schedule.
- **Climate mode is `off`**: zone shown as "not controlled"; no writes.
- **Restart mid-boost**: resumed from persisted `ends_at`.
- **Bad schedule input from UI**: validated server-side (time format, temp
  range); rejected with 400 and an error message.

## 10. Security / failure posture

- Ingress entry point = authenticated by HA; no secrets in the page.
- **Stable LAN port = unauthenticated** (§3.3): control is open to anyone on the
  LAN. Accepted LAN-trust trade-off; keep the port off the WAN. Since both entry
  points hit the same routes, never put secrets behind these endpoints.
- All handlers under `pcall`; errors → `ha.on_exception`; a handler timeout →
  HTTP 503, never a hung dashboard.
- Schedule/boost writes are validated before persisting.

## 11. Implementation milestones

1. **HTTP server core** — daemon HTTP server + `ha.serve` Lua API + the
   request→script-goroutine dispatch with timeout and pcall. Unit tests with a
   real LState. (No thermostat logic yet.)
2. **Packaging** — `config.yaml` ingress fields + `ports:` mapping for the
   stable LAN port; verify the sidebar panel loads and the Webpage-card URL
   works.
3. **Controller** — `desired()` engine, 1-minute tick, schedule + boost +
   manual-override persistence, and publishing `global:thermostat:desired:<zone>`
   (writes skipped while a window is open). Tests.
4. **Window cooperation** — update `heating_windows.lua` to drop its
   save-the-setpoint logic and instead restore the published desired on close
   (§4.2). Tests for the two-script handoff.
5. **HTTP API** — state/boost/schedule/settings endpoints on top of the
   controller.
6. **UI** — the single-page HTML/JS, polling, boost buttons, schedule editor.
7. **Docs** — DOCS.md section + CHANGELOG entry.

## 12. Open decisions (consolidated)

All resolved — ready to build.

- §2.1 UI effort hierarchy (boost = hero; per-zone comfort stepper; today
  shown; editor hidden).
- §3.3 Two entry points: Ingress (authenticated sidebar) + stable LAN port
  (dashboard Webpage card + dev). LAN port is unauthenticated (LAN-trust).
- §4.2 `heating_windows.lua` is **kept** and made schedule-aware; it cooperates
  with the controller via `global:thermostat:desired:<zone>` and the controller
  never writes while a window is open. Windows can no longer override the
  schedule.
- §4.3 Setpoint only; controller never manages hvac mode.
- §5.4 Comfort temp is **per zone**, UI-settable (store), not static config.
- §9 Manual setpoint change = ad-hoc override until the next schedule
  transition; UI boost outranks it.
- Defaults locked: frost 15 °C, default_comfort 21 °C, presets 10/30/60,
  poll 5 s, tick 1 min, ingress_port 8099, http_port 8100.
