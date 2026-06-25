# Thermostat dashboard card — HA-configured zones spec

Status: proposed (2026-06-25, rev 3 — card-provisioned dynamic zones)
Scope: a general daemon capability (`ha.set_state`/`ha.remove_state`/
`ha.on_command`), a **rework of the thermostat controller** to runtime-defined
zones, and the **`custom:ha-lua-thermostat-card`** interface. The card JS is a
separate deliverable; this spec defines the contract it relies on.

## 1. Goal

A dashboard card whose configuration is essentially **just the climate entity**.
Dropping the card on a dashboard and pointing it at `climate.living_room`:

- **provisions** that zone in the daemon (no `lib/zones.lua` edit),
- gives a working **7-day schedule editor**,
- gives **boost / timed override** + cancel,
- optionally binds a **window sensor** that pauses heating while open,

…all configured from HA, never by editing a script file.

## 2. Division of responsibility

The scheduling loop must run server-side (a browser card can't hold a 1-minute
control loop), but the *zone definitions now come from HA at runtime*:

| Concern | Owner | Lives in |
|---------|-------|----------|
| Which climate entity is a zone, its window sensor, boost presets | **Card config** | dashboard YAML, mirrored to the daemon on load |
| Schedule, active override, window state | **Daemon** | per-zone store (schedule already is) |
| Control loop (desired = override > manual > schedule; write climate) | **Daemon** | `thermostat.lua` |
| Live temp / setpoint / hvac_action / min/max | **HA** | the climate entity itself |
| Schedule/boost/override/window status the card needs | **Daemon** | companion `sensor.ha_lua_thermostat_<slug>` |

Identity: **a zone *is* a climate entity.** Key/slug = the climate entity's
object id (`climate.living_room` → `living_room`).

## 3. End-to-end flow

```
dashboard YAML  ──set_config──▶  card
  climate_entity, window_sensor?, presets?

card (on load / config change)
  ── fire ha_lua_command {action:"configure_zone",
       data:{key, climate_entity, window_sensor, presets}} ──▶ daemon
  (idempotent upsert; no-op if unchanged)

daemon thermostat.lua
  upsert zone in store ▶ control loop now includes it
  ▶ ha.set_state("sensor.ha_lua_thermostat_<slug>", target,
       {ha_lua_script="thermostat", ha_lua_climate=<entity>, ha_lua_key=<slug>,
        schedule, override, manual, window, presets, controlled})

card render = climate entity (temp/hvac/min/max)
            + companion sensor (schedule/boost/override/window)
  discovers the companion by attr ha_lua_climate == climate_entity

user edits (schedule save / boost / cancel / override temp)
  ── fire ha_lua_command {action, data:{key, ...}} ──▶ daemon
  daemon persists + re-publishes ▶ card reconciles from next hass update
```

The card config asks only for the climate entity; the companion sensor is
**discovered**, not configured.

## 4. Generic transport (unchanged from rev 2, still the foundation)

### 4.1 `ha.set_state` / `ha.remove_state` (new daemon capability)

ha-lua only ingests state today; publishing needs the core REST API.

- `internal/ha`: `Client.SetState(ctx, entityID, state, attrs) (created bool, err)`
  → `POST {restURL}/states/{id}` (200/201); `RemoveState` → `DELETE` (200/404 ok).
  ≈10s `http.Client`; shares the WS token.
- `internal/config`: add `HomeAssistant.RestURL`. Add-on mode forces
  `http://supervisor/core/api` (same place `URL`/`Token`/`IngressPort` are
  forced, `config.go:117`); `homeassistant_api: true` already grants it
  (`config.yaml:14`), no manifest/schema change. Dev derives from `URL`
  (`ws→http`/`wss→https`, strip trailing `/websocket`).
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
- `lib/card.lua` ergonomic wrapper so a script opts in cheaply:
  ```lua
  local card = require("card").new()           -- bound to this script's id
  card.on("schedule", function(d) set_schedule(d.key, d.schedule) end)
  card.publish(key, state, attrs)              -- stamps ha_lua_* markers, set_state
  card.remove(key)                             -- remove_state
  ```

These are generic: any future script (lights, etc.) reuses them. The thermostat
is the first consumer.

## 5. Command contract (card → daemon)

`ha_lua_command`, `script:"thermostat"`, dispatched on `action`; `data.key`
(the zone slug) on every command:

```jsonc
// provisioning — idempotent upsert, fired by the card on load/config change
{ "action":"configure_zone", "data":{ "key":"living_room",
    "climate_entity":"climate.living_room",
    "window_sensor":"binary_sensor.living_window",   // optional, "" to clear
    "presets":[10,30,60] } }                         // boost minutes, optional
{ "action":"remove_zone", "data":{ "key":"living_room" } }

// runtime edits
{ "action":"settings", "data":{ "key":"living_room", "override_temp":21.3 } }
{ "action":"override", "data":{ "key":"living_room", "minutes":30 } }
{ "action":"override", "data":{ "key":"living_room", "cancel":true } }
{ "action":"schedule", "data":{ "key":"living_room", "schedule":[ /* entries */ ] } }
```

Field shapes mirror the existing HTTP API so the Ingress UI and the card share
validation + mutators. Unknown action / unknown key → no-op. Every handler
validates → mutates iff valid → **re-publishes** the companion sensor;
rejected commands leave state unchanged and the card snaps back from `hass`
(optimism-free, the lesson the Ingress UI already learned). `configure_zone`
upserts and only restarts/republishes when the config actually changed (guards
against multi-tab/dashboard thrash).

## 6. Companion entity (daemon → card)

`sensor.ha_lua_thermostat_<slug>`, published on configure / load / 1-min tick /
every mutation:

- **state**: current desired setpoint (°C) when controlled, else `"off"`.
- **attributes**: `ha_lua_script:"thermostat"`, `ha_lua_climate:<entity>`,
  `ha_lua_key:<slug>`, `friendly_name`, `schedule` (7-day entries — small),
  `override` (`{active, expires, temp}`), `manual` (`{active}`),
  `window` (`{sensor, open}`), `presets`, `min_temp`, `max_temp`, `controlled`,
  plus `unit_of_measurement:"°C"`, `device_class:temperature`, `icon:mdi:thermostat`.

**Restart transience:** REST-set states aren't integration-backed, so an HA
restart drops them; the ≤1-min tick + reconnect-reload re-publish, so they
self-heal. Documented caveat. `remove_zone` calls `ha.remove_state` so a removed
zone's sensor disappears.

## 7. Controller rework (`thermostat.lua`) — the big change

Today zones come from static `lib/zones.lua`; they now come from the store.

1. **Zone registry in the store** (global-scoped so the Ingress UI and card share
   one source), keyed by slug: `{climate_entity, window_sensor, presets}`. CRUD
   via the `configure_zone` / `remove_zone` handlers (§5).
2. **Control loop iterates store zones** instead of `zones.lua`. Per-zone
   `desired() = override > manual > schedule`, writing the climate entity only
   when `mode==heat`, no bound window open, and the value changed (>0.05) — the
   existing logic, just over a dynamic zone set. `temp_bounds(zone)` still reads
   the climate entity's `min_temp`/`max_temp` so the card can't push a setpoint
   HA silently drops (AI.state 2.3.0).
3. **Window cooperation folds in.** The per-zone `window_sensor` comes from
   config; the loop checks its state each tick and pauses/restores heating
   (replacing the separate `heating_windows.lua` zones.lua coupling). For
   immediacy, also subscribe `ha.on_state_change("binary_sensor.*", …)` and
   filter to configured window sensors so an opened window pauses within seconds,
   not up to a minute. **Assumption to confirm:** "optional window sensor
   disablement" = bind a sensor that pauses heating while open (omit to disable
   the feature); not a separate on/off toggle.
4. **Companion publish** (§6) on configure / load / tick / mutation.
5. **Retire `lib/zones.lua`** and its placeholder ids. The schedule store layout
   is unchanged (already per-zone in the store), so only the *zone list* source
   moves. `heating_windows.lua` is folded in / retired.

This is **BREAKING** for anyone running the example as-is (zones.lua goes away) →
**major** version bump (release process §SemVer).

## 8. Ingress UI alignment

The Ingress UI already edits store-backed schedules; it now also reads the
dynamic zone set (zones appear as cards provision them). It should gain an
**add-zone** (pick a climate entity) and **remove-zone** flow so users who don't
use the dashboard card, and orphan cleanup, are both covered (a card removed from
a dashboard can't send `remove_zone`, so the zone lingers until removed here).
Shares the same store + mutators as the command handlers.

## 9. Card interface (`custom:ha-lua-thermostat-card`, separate deliverable)

```yaml
type: custom:ha-lua-thermostat-card
climate_entity: climate.living_room        # required — the only must-have
window_sensor: binary_sensor.living_window # optional
presets: [10, 30, 60]                      # optional boost minutes
name: Living room                          # optional; else friendly_name
```

- On `setConfig` / first `hass`: fire `configure_zone` (idempotent) so adding the
  card provisions the zone.
- Discover the companion sensor by `attributes.ha_lua_climate === climate_entity`.
- Render from the **climate entity** (current temp, hvac_action, min/max) + the
  **companion** (schedule, override/boost, window): status line, target/override
  stepper, boost preset row + live countdown, the **7-day schedule editor**, and
  a window-state indicator when bound.
- All mutations fire `ha_lua_command`; **optimism-free** — re-render from the next
  `hass` update, never from local writes.
- A config editor (`getConfigElement`) with an entity picker for
  `climate_entity` / `window_sensor` makes it fully GUI-configurable.

## 10. Testing

**Go — generic transport:** `SetState`/`RemoveState` against `httptest`
(method/path/auth/body, 200/201/404, ctx-cancel); `config_test` RestURL force +
derive; capturing-stub tests for `ha.set_state`/`ha.on_command` (non-raising).

**Go — controller (via `Runner` like `TestThermostatAPI`):**
1. `configure_zone` creates a zone, starts control, publishes the companion;
   re-`configure_zone` with same config is a no-op; changed config updates.
2. `remove_zone` stops control and `remove_state`s the companion.
3. Control loop drives a *store-defined* zone (no zones.lua): schedule/override
   priority, write-gating on mode/window/changed.
4. Window pause/restore from a dynamically-bound sensor.
5. Command round-trips: `settings` (bounds-rejected out of range), `schedule`
   round-trip, `override` start/cancel — each re-publishes the companion.

No browser test (card JS out of scope). Green under `-race` + `make check`.

## 11. Milestones / commits (each compiles + `make test`)

**M1 — generic transport (reusable daemon):**
1. `ha: add core REST client for entity set/remove`
2. `config: derive and force the core REST API base URL`
3. `lua: add ha.set_state / ha.remove_state / ha.on_command + lib/card.lua`

**M2 — dynamic-zone controller (examples + the breaking change):**
4. `examples: store-backed thermostat zone registry + configure/remove`
5. `examples: drive the control loop from runtime zones, retire zones.lua`
6. `examples: fold window cooperation into per-zone config`
7. `examples: publish thermostat companion sensors`

**M3 — Ingress UI:** 8. `examples: add/remove zones from the Ingress UI`

**M4 — card (separate deliverable):** the `custom:ha-lua-thermostat-card` JS +
config editor, and a dashboard-resource install note in DOCS.

**M5 — docs/release:** DOCS (`ha.set_state`/`on_command`, the card config, the
entity model, the zones.lua removal/migration, restart caveat) + CHANGELOG with a
bold **BREAKING** lead + **major** version bump.

> M1 commits ship in the binary and are reusable by any script. M2–M3 are
> examples-only (reference tree, never loaded — AI.state); the user's live
> `/config/ha-lua/scripts/` must be migrated off `zones.lua` separately. M4 is
> the only non-Lua/Go deliverable.

## 12. Open items

- **Window-sensor semantics** (§7.3) — confirm "bind a sensor, open pauses
  heating" vs a separate enable/disable toggle.
- **Orphan zones** — a card deleted from a dashboard leaves its zone; cleanup is
  via the Ingress UI (§8) or a future TTL. Acceptable for v1.
- **Multiple cards, one climate entity** — idempotent `configure_zone` makes this
  safe; last writer of `window_sensor`/`presets` wins (cosmetic).
