# Enhanced-climate example & card — HA-configured spec

Status: proposed (2026-06-25, rev 5 — standalone new example, not a rework)
Scope: a general daemon capability (`ha.set_state`/`ha.remove_state`/
`ha.on_command` + `lib/card.lua`), a **new standalone example**
(`examples/enhanced_climate.lua`) that uses it, and the
**`custom:ha-lua-enhanced-climate-card`** interface. The card JS is a separate
deliverable; this spec defines the contract it relies on.

## 0. Terminology & relationship to the existing example

An **enhanced climate** is one HA `climate` entity wrapped with ha-lua's
scheduling, boost/timed override, manual-override detection, and optional
window-sensor cooperation — an enhanced climate entity. It is keyed by its
climate entity id.

This is a **new, standalone example**, parallel to the existing, working
`examples/thermostat.lua`. **Nothing about the existing example changes** — its
`lib/zones.lua` static model, its Ingress editor UI, and all its tests stay as
they are. The two are alternative references:

| | `thermostat.lua` (existing) | `enhanced_climate.lua` (new) |
|--|--|--|
| Zones defined in | `lib/zones.lua` (edit + redeploy) | HA card config (runtime) |
| Edited via | Ingress web UI | HA dashboard card |
| Surfaces state as | Ingress page only | published HA entities |

Because the new example is additive, the release is a **minor** bump (new Lua
APIs + a new example), **not** the breaking major an in-place rework would have
been. The pure `lib/schedule.lua` (resolve/validate) is reused by both; only the
controller wiring differs.

## 1. Goal

A dashboard card whose configuration is essentially **just the climate entity**.
Dropping the card on a dashboard and pointing it at `climate.living_room`:

- **provisions** that enhanced climate in the daemon (no script edit),
- gives a working **7-day schedule editor**,
- gives **boost / timed override** + cancel,
- optionally binds a **window sensor** that pauses heating while open,
- and **replaces a native `tile` climate card** — it must also do everything the
  tile did (current temp/state, target-temperature, HVAC modes).

…all configured from HA, never by editing a script file.

## 2. Division of responsibility

The scheduling loop must run server-side (a browser card can't hold a 1-minute
control loop), but the *definitions now come from HA at runtime*:

| Concern | Owner | Lives in |
|---------|-------|----------|
| Which climate is enhanced, its window sensor, boost presets | **Card config** | dashboard YAML, mirrored to the daemon on load |
| Schedule, active override, window state | **Daemon** | per-climate store |
| Control loop (desired = override > manual > schedule; write climate) | **Daemon** | `enhanced_climate.lua` |
| Live temp / setpoint / hvac_action / min/max / mode | **HA** | the climate entity itself |
| Schedule/boost/override/window status the card needs | **Daemon** | companion `sensor.ha_lua_enhanced_climate_<slug>` |

Identity: an enhanced climate **is** a climate entity. Slug = the climate
entity's object id (`climate.living_room` → `living_room`).

## 3. End-to-end flow

```
dashboard YAML  ──setConfig──▶  card
  climate_entity, window_sensor?, presets?

card (on load / config change)
  ── fire ha_lua_command {script:"enhanced_climate", action:"configure",
       data:{ climate_entity, window_sensor, presets }} ──▶ daemon
  (idempotent upsert; no-op if unchanged)

daemon enhanced_climate.lua
  upsert in store ▶ control loop now includes this climate
  ▶ ha.set_state("sensor.ha_lua_enhanced_climate_<slug>", target,
       { ha_lua_script="enhanced_climate", ha_lua_climate=<climate_entity>,
         schedule, override, manual, window, presets, controlled, removal })

card render = climate entity (temp/hvac/mode/min/max, via native services)
            + companion sensor (schedule/boost/override/window/manual)
  discovers the companion by attr ha_lua_climate == climate_entity

user edits enhanced bits (schedule / boost / cancel / override_temp)
  ── fire ha_lua_command {action, data:{ climate_entity, ... }} ──▶ daemon
  daemon persists + re-publishes ▶ card reconciles from next hass update
```

The card config asks only for the climate entity; the companion is **discovered**,
not configured.

## 4. Generic transport (reusable daemon foundation)

### 4.1 `ha.set_state` / `ha.remove_state` (new capability)

ha-lua only ingests state today; publishing needs the core REST API.

- `internal/ha`: `Client.SetState(ctx, entityID, state, attrs) (created bool, err)`
  → `POST {restURL}/states/{id}` (200/201); `RemoveState` → `DELETE` (200/404 ok).
  ≈10s `http.Client`; shares the WS token.
- `internal/config`: add `HomeAssistant.RestURL`. Add-on mode forces
  `http://supervisor/core/api` (where `URL`/`Token`/`IngressPort` are forced,
  `config.go:117`); `homeassistant_api: true` already grants it (`config.yaml:14`),
  no manifest/schema change. Dev derives from `URL` (`ws→http`/`wss→https`, strip
  trailing `/websocket`).
- `internal/lua`: `Deps.SetState`/`RemoveState`, bindings `ha.set_state`/
  `ha.remove_state`, **non-raising** (`value|nil, err`, like `http`/`fs`) so the
  per-minute publish doesn't spam `on_exception` during a transient outage.

> Rejected: a Python custom integration to register entities/services — a second
> codebase in a second language for the same result.

### 4.2 `ha.on_command` + `lib/card.lua`

One inbound event for everything: `ha_lua_command` `{script, action, data}`.

- `ha.on_command(handler)` binding: wraps `ha.on_event("ha_lua_command", …)`,
  filters `data.script == api.scriptID` (the runner already knows the id — it's
  how `ha.log` tags lines, `api_ha.go`), calls `handler(action, data)`.
- `lib/card.lua` ergonomic wrapper:
  ```lua
  local card = require("card").new{ kind = "enhanced_climate" }  -- publish prefix
  card.on("schedule", function(d) set_schedule(d.climate_entity, d.schedule) end)
  card.publish(slug, state, attrs)   -- sensor.ha_lua_<kind>_<slug>, stamps markers
  card.remove(slug)                  -- remove_state
  ```
  `kind` sets the published-entity prefix (defaults to the script id; here it
  equals the script id anyway). `data` is passed through verbatim, so the helper
  stays generic — it mandates no field shape, only `script` routing + the
  `ha_lua_script` marker. Reusable by any future card-driven script.

## 5. Command contract (card → daemon)

`ha_lua_command`, `script:"enhanced_climate"`, dispatched on `action`; every
command identifies its target with `data.climate_entity`:

```jsonc
// provisioning — idempotent upsert, fired by the card on load/config change
{ "action":"configure", "data":{
    "climate_entity":"climate.living_room",
    "window_sensor":"binary_sensor.living_window",   // optional, "" to clear
    "presets":[10,30,60] } }                         // boost minutes, optional
{ "action":"remove", "data":{ "climate_entity":"climate.living_room" } }

// runtime edits (enhanced layer only — temp/mode go via native climate services, §9)
{ "action":"settings", "data":{ "climate_entity":"climate.living_room", "override_temp":21.3 } }
{ "action":"override", "data":{ "climate_entity":"climate.living_room", "minutes":30 } }
{ "action":"override", "data":{ "climate_entity":"climate.living_room", "cancel":true } }
{ "action":"schedule", "data":{ "climate_entity":"climate.living_room", "schedule":[ /* entries */ ] } }
```

Actions need no noun suffix — `script` already scopes them. Unknown action /
unknown climate → no-op. Every handler validates → mutates iff valid →
**re-publishes** the companion; rejected commands leave state unchanged and the
card snaps back from `hass` (optimism-free). `configure` upserts and only
restarts/republishes when the config actually changed (guards multi-tab thrash).

## 6. Companion entity (daemon → card)

`sensor.ha_lua_enhanced_climate_<slug>`, published on configure / load / 1-min
tick / every mutation:

- **state**: current desired setpoint (°C) when controlled, else `"off"`.
- **attributes**: `ha_lua_script:"enhanced_climate"`,
  `ha_lua_climate:<climate_entity>` (the discovery key), `friendly_name`,
  `schedule` (7-day entries), `override` (`{active, expires, temp}`),
  `manual` (`{active, until}`), `window` (`{sensor, open}`), `presets`,
  `min_temp`, `max_temp`, `controlled`, plus `unit_of_measurement:"°C"`,
  `device_class:temperature`, `icon:mdi:thermostat`.
- one subtle removal-pointer attribute, e.g.
  `removal: "Deleting the card keeps this running — remove it in the ha-lua panel"`,
  so a user on the entity's **more-info** screen who wonders "how do I get rid of
  this?" finds the answer. One attribute line: a rare action (§8), kept
  understated rather than a banner or custom more-info component.

**Restart transience:** REST-set states aren't integration-backed, so an HA
restart drops them; the ≤1-min tick + reconnect-reload re-publish, so they
self-heal. `remove` calls `ha.remove_state` so a removed climate's sensor
disappears.

## 7. New example controller (`examples/enhanced_climate.lua`)

Built fresh (not a rework of `thermostat.lua`), reusing the pure `lib/schedule.lua`
and the new `lib/card.lua`:

1. **Registry in the store** (global-scoped), keyed by climate entity id:
   `{climate_entity, window_sensor, presets}`. CRUD via the `configure` / `remove`
   handlers (§5).
2. **1-min `ha.every` loop iterates the registry.** Per climate,
   `desired() = override > manual > schedule` (schedule via `lib/schedule.lua`
   resolve), writing via `climate.set_temperature` only when `mode==heat`, no
   bound window open, and the value changed (>0.05). Manual detection: a climate
   target differing from the published desired (>0.1) starts a manual hold until
   the next schedule transition. `temp_bounds()` reads the climate's
   `min_temp`/`max_temp` so nothing pushes a setpoint HA silently drops
   (AI.state 2.3.0).
3. **Window cooperation built in.** The per-climate `window_sensor` (from config)
   is checked each tick; for immediacy also `ha.on_state_change("binary_sensor.*", …)`
   filtered to configured sensors, so an opened window pauses heating within
   seconds and a close restores it.
4. **Companion publish** (§6) on configure / load / tick / mutation.
5. **Boost / override:** store `{expires, temp}`; presets from config;
   `override_temp` is the target a boost jumps to (edited via `settings`).

This duplicates some logic from `thermostat.lua` by design — the working example
stays untouched and the shared pure parts live in `lib/schedule.lua`. If a third
consumer appears, factor the common controller bits into a lib then (not before).

## 8. Lifecycle & removal (this example's Ingress page)

An enhanced climate is **persistent config** — it outlives any card. Provisioning
is the idempotent `configure`; **removal is explicit, in this example's own small
Ingress page**, which lists the registry with a remove button → the `remove`
handler. That one place covers deliberate teardown and orphans alike (a card
deleted from a dashboard can't send `remove`). This is a *new, minimal* page for
the new example — separate from the existing `thermostat.lua` Ingress editor,
which is untouched. Most editing now lives in the card, so this page is little
more than the list + remove.

Removal is deliberately **not** automatic. A card heartbeat / TTL was rejected: a
Lovelace card's JS only runs while its dashboard view is open, so "no keepalive
for N days" measures whether someone is *looking*, not whether the config is
wanted — an unopened dashboard (phone-only control, a vacation house) would get
its schedule silently reaped. Reading the Lovelace config to reconcile cards
couples the daemon to frontend internals and can't see YAML-mode dashboards. One
explicit surface is simpler and has no silent-failure mode. Because deleting a
card therefore does *not* remove the enhanced climate, the companion carries the
subtle more-info pointer (§6).

## 9. Card interface (`custom:ha-lua-enhanced-climate-card`, separate deliverable)

```yaml
type: custom:ha-lua-enhanced-climate-card
climate_entity: climate.living_room        # required — the only must-have
window_sensor: binary_sensor.living_window # optional
presets: [10, 30, 60]                      # optional boost minutes
name: Living room                          # optional; else friendly_name
```

The card **replaces a native `tile` climate card** (e.g. `type: tile` with
`target-temperature` + `climate-hvac-modes` features), so it must cover that
*and* the enhanced layer. The split is deliberate:

**Climate-native controls — reuse the native climate services, not commands:**
- current temperature + state + `hvac_action` — read from the climate entity.
- **target temperature** → `climate.set_temperature` (the tile's
  `target-temperature` feature). No custom command: a setpoint change ≠ published
  desired *is* the daemon's manual-hold signal (until the next schedule
  transition, §7.2). The card shows a "held until HH:MM" badge from the
  companion's `manual`.
- **HVAC mode** → `climate.set_hvac_mode` (the tile's `climate-hvac-modes`
  feature). The daemon gates control on `mode==heat`.

These reconcile from `hass.states[climate_entity]`, exactly as the tile does, so
they keep working even if the daemon is briefly down (it re-asserts the schedule
when it returns).

**Enhanced controls — `ha_lua_command` + the companion sensor:**
- boost / timed-override preset row + live countdown + cancel,
- the **7-day schedule editor**,
- the **override-temp** setting (the temp a boost jumps to),
- a window-state indicator when bound.

These reconcile from the companion (`ha_lua_climate === climate_entity`) and are
**optimism-free** — re-render from the next `hass` update, never local writes.

**Lifecycle / config:**
- On `setConfig` / first `hass`: fire `configure` (idempotent) so adding the card
  provisions the enhanced climate.
- A config editor (`getConfigElement`) with entity pickers for `climate_entity`
  and `window_sensor` makes it fully GUI-configurable — only `climate_entity` is
  required.

## 10. Testing

**Go — generic transport:** `SetState`/`RemoveState` against `httptest`
(method/path/auth/body, 200/201/404, ctx-cancel); `config_test` RestURL force +
derive; capturing-stub tests for `ha.set_state`/`ha.on_command` (non-raising).

**Go — new example (via `Runner`, mirroring `TestThermostatAPI`):**
1. `configure` creates an enhanced climate, starts control, publishes the
   companion; re-`configure` same config is a no-op; changed config updates.
2. `remove` stops control and `remove_state`s the companion.
3. Control loop drives a store-defined climate: schedule/override priority,
   write-gating on mode/window/changed, manual-hold detection.
4. Window pause/restore from a dynamically-bound sensor.
5. Command round-trips: `settings` (bounds-rejected out of range), `schedule`
   round-trip, `override` start/cancel — each re-publishes the companion.

The existing `thermostat.lua` tests are untouched. No browser test (card JS out
of scope). Green under `-race` + `make check`.

## 11. Milestones / commits (each compiles + `make test`)

**M1 — generic transport (reusable daemon, ships in the binary):**
1. `ha: add core REST client for entity set/remove`
2. `config: derive and force the core REST API base URL`
3. `lua: add ha.set_state / ha.remove_state / ha.on_command + lib/card.lua`

**M2 — the new example (additive, examples-only):**
4. `examples: enhanced_climate registry + configure/remove handlers`
5. `examples: enhanced_climate control loop + manual-hold + bounds`
6. `examples: enhanced_climate window cooperation`
7. `examples: enhanced_climate companion-sensor publishing`

**M3 — removal UI:** 8. `examples: enhanced_climate Ingress removal page`

**M4 — card (separate deliverable):** `custom:ha-lua-enhanced-climate-card` JS +
config editor + a dashboard-resource install note in DOCS.

**M5 — docs/release:** DOCS (`ha.set_state`/`on_command`, the card config, the
entity model, restart caveat) + CHANGELOG + **minor** version bump (new APIs + new
example; existing example untouched, nothing breaks).

> M1 ships in the binary and is reusable by any script. M2–M3 are examples-only
> (reference tree, never loaded — AI.state); a user copies `enhanced_climate.lua`
> into `/config/ha-lua/scripts/` to use it. The existing `thermostat.lua` is left
> entirely alone. M4 is the only non-Lua/Go deliverable.

## 12. Open items

- **Multiple cards, one climate entity** — idempotent `configure` makes this
  safe; last writer of `window_sensor`/`presets` wins (cosmetic).
- **Example name** — `enhanced_climate.lua` chosen for consistency with the
  concept/entity/card naming; rename if preferred before M2.
