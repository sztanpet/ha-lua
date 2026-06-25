# Enhanced-climate example & card — HA-configured spec

> **Working state:** [`state/enhanced-climate.md`](state/enhanced-climate.md) — implementation progress, decisions, release log.

Status: proposed (2026-06-25, rev 8 — shared lib/control.lua, multiple window
sensors, direct companion lookup; card mirrors thermostat.html; i18n in v1)
Scope: a general daemon capability (`ha.set_state`/`ha.remove_state`/
`ha.on_command` + `lib/card.lua`), a **new standalone example**
(`examples/enhanced_climate.lua`) that uses it, the
**`custom:ha-lua-enhanced-climate-card`** contract (§9), and the **card UI
implementation** (§10). The card is a bundled vanilla JS asset materialized into
`/config/www`.

## 0. Terminology & relationship to the existing example

An **enhanced climate** is one HA `climate` entity wrapped with ha-lua's
scheduling, boost/timed override, manual-override detection, and optional
window-sensor cooperation — an enhanced climate entity. It is keyed by its
climate entity id.

This is a **new, standalone example**, parallel to the existing, working
`examples/thermostat.lua`. The existing example's **behavior, UI, and tests are
unchanged** — its `lib/zones.lua` static model and its Ingress editor UI stay as
they are; only the shared pure control helpers it already contains are
mechanically lifted into `lib/control.lua` (§7.1), a behavior-preserving refactor
with its own commit. The two are alternative references:

| | `thermostat.lua` (existing) | `enhanced_climate.lua` (new) |
|--|--|--|
| Zones defined in | `lib/zones.lua` (edit + redeploy) | HA card config (runtime) |
| Edited via | Ingress web UI | HA dashboard card |
| Surfaces state as | Ingress page only | published HA entities |

Because the new example is additive, the release is a **minor** bump (new Lua
APIs + a new example), **not** the breaking major an in-place rework would have
been. The pure `lib/schedule.lua` (resolve/validate) and the new pure
`lib/control.lua` (desired-priority, bounds clamp, manual detection, window
gating) are shared by both controllers; only the impure wiring differs.

## 1. Goal

A dashboard card whose configuration is essentially **just the climate entity**.
Dropping the card on a dashboard and pointing it at `climate.living_room`:

- **provisions** that enhanced climate in the daemon (no script edit),
- gives a working **7-day schedule editor**,
- gives **boost / timed override** + cancel,
- optionally binds **one or more window sensors** that pause heating while any
  is open (heating resumes only once all are closed),
- and **replaces a native `tile` climate card** — it must also do everything the
  tile did (current temp/state, target-temperature, HVAC modes).

…all configured from HA, never by editing a script file.

## 2. Division of responsibility

The scheduling loop must run server-side (a browser card can't hold a 1-minute
control loop), but the *definitions now come from HA at runtime*:

| Concern | Owner | Lives in |
|---------|-------|----------|
| Which climate is enhanced, its window sensors, boost presets | **Card config** | dashboard YAML, mirrored to the daemon on load |
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
       data:{ climate_entity, window_sensors, presets }} ──▶ daemon
  (idempotent upsert; no-op if unchanged)

daemon enhanced_climate.lua
  upsert in store ▶ control loop now includes this climate
  ▶ ha.set_state("sensor.ha_lua_enhanced_climate_<slug>", target,
       { ha_lua_script="enhanced_climate", ha_lua_climate=<climate_entity>,
         schedule, override, manual, window, presets, controlled, removal })

card render = climate entity (temp/hvac/mode/min/max, via native services)
            + companion sensor (schedule/boost/override/window/manual)
  looks the companion up by its derived id sensor.ha_lua_enhanced_climate_<slug>

user edits enhanced bits (schedule / boost / cancel / override_temp)
  ── fire ha_lua_command {action, data:{ climate_entity, ... }} ──▶ daemon
  daemon persists + re-publishes ▶ card reconciles from next hass update
```

The card config asks only for the climate entity; the companion's id is
**derived** from it (slug = the climate object id), not configured.

## 4. Generic transport (reusable daemon foundation)

### 4.1 `ha.set_state` / `ha.remove_state` (new capability)

ha-lua only ingests state today; publishing needs the core REST API.

- `internal/ha`: `Client.SetState(ctx, entityID, state, attrs) (created bool, err)`
  → `POST {restURL}/states/{id}` (200/201); `RemoveState` → `DELETE` (200/404 ok).
  ≈10s `http.Client`; shares the WS token.
- The REST base is **derived from the WS URL inside `ha.New`**, not a config
  field: the two endpoints share a host but differ in scheme (`ws→http`/
  `wss→https`) and path tail. After dropping a trailing `/websocket` the base is
  normalised to end in `/api`, which maps both real forms —
  `ws://supervisor/core/websocket → http://supervisor/core/api` (add-on, the
  Supervisor WS path lacks the `/api` REST lives under) and
  `ws://host:8123/api/websocket → http://host:8123/api` (dev). No
  `config`/manifest/schema change; `homeassistant_api: true` already grants the
  core REST API (`config.yaml:14`).
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
    "window_sensors":["binary_sensor.living_window"], // optional, [] to clear
    "presets":[10,30,60] } }                          // boost minutes, optional
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
  `ha_lua_climate:<climate_entity>` (a sanity marker, not the lookup key — the
  card derives the id directly, §10.3), `friendly_name`,
  `schedule` (7-day entries), `override` (`{active, expires, temp}`),
  `manual` (`{active, until}`), `window` (`{sensors, open}` — `open` = any bound
  sensor open), `presets`,
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
disappears. The per-minute republish is **value-stable** (state = desired °C,
attributes carry only fixed timestamps like `override.expires`/`manual.until`),
so HA suppresses the no-op `state_changed` and the recorder is not bloated; DOCS
still recommend a recorder `exclude` for `sensor.ha_lua_enhanced_climate_*` as
belt-and-suspenders.

## 7. New example controller (`examples/enhanced_climate.lua`)

Built fresh (not a rework of `thermostat.lua`), reusing the pure
`lib/schedule.lua`, the new pure `lib/control.lua` (§7.1), and `lib/card.lua`:

1. **Registry in the store** (global-scoped), keyed by climate entity id:
   `{climate_entity, window_sensors, presets}`. CRUD via the `configure` /
   `remove` handlers (§5).
2. **1-min `ha.every` loop iterates the registry.** Per climate,
   `control.desired(override, manual, schedule_temp)` picks `override > manual >
   schedule` (schedule via `lib/schedule.lua` resolve), writing via
   `climate.set_temperature` only when `control.should_write(mode, window_open,
   current, target)` holds (`mode==heat`, no bound window open, value changed
   >0.05). Manual detection: `control.is_manual(target, published_desired)` — a
   climate target differing from the published desired (>0.1) starts a manual
   hold until the next schedule transition. `control.clamp_bounds` against the
   climate's `min_temp`/`max_temp` so nothing pushes a setpoint HA silently
   drops (AI.state 2.3.0).
3. **Multi-sensor window cooperation built in.** Each climate's `window_sensors`
   list (from config) is reduced by `control.window_open(states)` — **open if
   any bound sensor is open, clear only when all are closed**. Checked each tick;
   for immediacy also `ha.on_state_change("binary_sensor.*", …)` filtered to
   configured sensors, so an opened window pauses heating within seconds and the
   last close restores it.
4. **Companion publish** (§6) on configure / load / tick / mutation.
5. **Boost / override:** store `{expires, temp}`; presets from config;
   `override_temp` is the target a boost jumps to (edited via `settings`).

### 7.1 Shared pure controller helpers (`lib/control.lua`)

The desired-priority pick, the bounds clamp, the manual-hold predicate, and the
window any-open/all-closed reduction are **identical** to what `thermostat.lua`
already does. Rather than copy them into the new example — which would leave two
drifting copies of hard-won logic (the silent out-of-range drop fix, the manual
tolerances) — they are lifted into a new **pure** `lib/control.lua` (no
`ha`/`store`/`time`, so Go-unit-testable like `lib/schedule.lua`):

```lua
control.desired(override, manual, schedule_temp)  -- priority pick, nil if none
control.is_manual(target, published_desired)      -- |Δ| > 0.1
control.should_write(mode, window_open, cur, tgt) -- gate set_temperature
control.clamp_bounds(value, lo, hi)               -- device min/max clamp
control.window_open(sensor_states)                -- any open / all closed
```

`thermostat.lua` is **mechanically migrated** onto these helpers in its own
behavior-preserving commit (§12) — its UI, its `lib/zones.lua` model, and all its
existing tests stay green; only the inline copies become `control.*` calls. The
impure wiring (registry CRUD, the `ha.every` loop, `set_temperature`, publishing)
stays per-example. This is real deduplication: one copy of each helper, exercised
by both controllers and by `lib/control`'s own unit tests.

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

## 9. Card contract (`custom:ha-lua-enhanced-climate-card`)

```yaml
type: custom:ha-lua-enhanced-climate-card
climate_entity: climate.living_room           # required — the only must-have
window_sensors: [binary_sensor.living_window] # optional, one or more
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

These reconcile from the companion (looked up by its derived id, §10.3) and are
**optimism-free** — re-render from the next `hass` update, never local writes.

**Lifecycle / config:**
- On `setConfig` / first `hass`: fire `configure` (idempotent) so adding the card
  provisions the enhanced climate.
- A config editor (`getConfigElement`) with an entity picker for `climate_entity`
  and a multi-entity picker for `window_sensors` makes it fully GUI-configurable
  — only `climate_entity` is required.

## 10. Card UI implementation (`ha-lua-enhanced-climate-card.js`)

**Guiding principle: the card mirrors `thermostat.html` functionality-wise.** The
existing Ingress UI is the reference for *what the card does* — the comfort/
override-temp stepper, the boost hero + presets + countdown, the today strip, the
per-day 7-day schedule editor with weekday regrouping, the status badge, the
not-controlled notice, and the hu/en localization. The card re-expresses those
same behaviors as a Lovelace card (driven by `hass` + the companion entity
instead of the polled HTTP API, with the climate-native controls of §9 added on
top). Port behavior and string keys from `thermostat.html`; don't reinvent them.
Where a behavior already has a hard-won fix (focus/optimism/ordering, AI.state),
carry the fix over rather than re-deriving it.

### 10.1 Tech & packaging

- **One self-contained vanilla file**, no build step, no runtime imports — a
  `customElements.define`d element `extends HTMLElement`, rendering with template
  strings + manual DOM. This matches the existing Ingress UI (`thermostat.html`
  is hand-written vanilla JS/CSS, AI.state) and keeps a Go/Lua repo free of an
  npm/bundler toolchain and of fragile `unpkg`/CDN `import`s. Lit is *not* used.
- Source lives at `ha-lua/cards/ha-lua-enhanced-climate-card.js` (reference asset).
- The add-on **materializes** it to `/config/www/ha-lua/enhanced-climate-card.js`
  on boot (best-effort, mirroring the examples `Materialize`; a forced
  `CardsDir = /config/www/ha-lua` in add-on mode, dev leaves it empty). It is then
  served at `/local/ha-lua/enhanced-climate-card.js`; DOCS instruct adding a
  dashboard **resource** (that URL, type `module`). No HACS dependency.
- On load: a `console.info` version banner (HA convention) and
  `window.customCards.push({type, name, description, preview:true})` so it shows
  in the "add card" picker.

### 10.2 Lifecycle methods

- `setConfig(config)`: require `climate_entity` (throw otherwise → HA renders the
  error card); stash config; precompute the companion lookup.
- `set hass(hass)`: stash, then a throttled re-render (rAF-coalesced). Fire
  `configure` (idempotent) whenever the **effective config changed** since the
  last send — keyed off a hash of `{climate_entity, window_sensors, presets}`, so
  it covers the first `hass`, a later `setConfig` GUI edit, and re-mounts alike;
  it is **not** a once-per-first-hass call (else an editor change to
  `window_sensors` would never reach the daemon). Driven from here, not from
  `setConfig`, because `callApi` needs `hass`.
- `getCardSize()`: ~5 (height hint).
- `static getConfigElement()` / `static getStubConfig()`: the GUI editor (§10.6)
  and a default (`{ climate_entity: "" }`) for the picker.

### 10.3 Data sources & reconciliation

- **Two reads:** `hass.states[climate_entity]` for current temp / target / mode /
  `hvac_action` / `min_temp` / `max_temp`; the companion by its **derived id**
  `hass.states['sensor.ha_lua_enhanced_climate_' + slug]` (slug = the climate
  object id) for schedule / override / manual / window / presets. No O(n) scan of
  all states; `ha_lua_climate` on the companion is only a sanity check.
- **Optimism-free**, re-render on every `hass`: never write `input.value`
  optimistically — reflect server truth, preserving focus + caret on the active
  input. Port the proven patterns from `thermostat.html` (no optimistic write,
  `lastSent` dedupe, blur/Enter commit, Escape revert) — those exist because the
  Ingress UI hit ordering/race bugs under `-race` without them (AI.state).
- A **local 1 s timer drives only the boost countdown** display (remaining derived
  from `override.expires`); all data comes from `hass` push, so there is **no
  polling** (unlike the Ingress UI's 5 s poll).
- Empty states: companion missing → "Setting up…" (configure fired, awaiting the
  sensor); climate entity `unavailable` → an unavailable notice.

### 10.4 Layout & controls (mirrors the §9 split)

- **Header:** name + status badge (on / heating / off from `hvac_action`+mode,
  reuse the existing `statusLabel` logic) + a "held until HH:MM" badge when
  `companion.manual.active`.
- **Climate-native:** target-temp stepper (±, typed input, clamped to
  min/max) → `set_temperature`; mode selector built from the entity's
  `hvac_modes` → `set_hvac_mode`. (Single-setpoint heating climates; dual
  `target_temp_high/low` range mode is out of scope for v1.)
- **Enhanced:** boost preset row + live countdown + cancel; `override_temp`
  stepper; the **7-day schedule editor** (port the existing editor's row model +
  weekday regrouping from `thermostat.html`); a read-only window indicator when
  any sensor is bound (open/closed from the any-open reduction).

### 10.5 Command & service helpers

- `fireCommand(action, data)` →
  `hass.callApi('POST', 'events/ha_lua_command', { script:'enhanced_climate', action, data:{ climate_entity, ...data } })`.
- `callClimate(service, data)` →
  `hass.callService('climate', service, { entity_id: climate_entity, ...data })`.
- Factor these plus the **pure** derivations (status label, remaining-time,
  schedule regroup, bounds clamp) as free functions so they are unit-testable
  without a browser (§10.8).

### 10.6 Config editor (`getConfigElement`)

A second element using HA's `ha-entity-picker` for `climate` (required) and
`ha-entities-picker` (plural) for the `window_sensors` list of `binary_sensor`s
(optional), plus a presets input and an optional `name`; dispatches
`config-changed`. Makes the card fully GUI-configurable — the only required field
is the climate entity. These HA helper elements are undocumented frontend
internals: the editor can only be exercised inside a live HA frontend (not the
§10.8 harness) and may need adjusting across HA frontend releases.

### 10.7 Localization (i18n)

First-class in v1. The card reads HA's user language from `hass.language` and
translates via an **embedded strings table** (English + Hungarian to start,
reusing the existing Ingress UI keys where the concepts overlap) with English
fallback for missing keys. A small `t(key, vars?)` helper does the lookup; **all**
user-visible text — status badges, the "held until HH:MM" badge, boost / schedule
/ window labels, weekday names, and the config-editor field labels — goes through
it, with no hard-coded strings.

This is simpler than the Ingress UI's scheme: because `hass.language` already
carries the user's choice, the card needs **no `?lang=` query param, no language
picker, and no localStorage** (all of which the Ingress UI has). Number/weekday/
time formatting uses the browser locale derived from `hass.language`.

### 10.8 Testing the card

Reuse the project's chromedp approach (the Ingress UI's browser tests), but with
a tiny static **harness page** the Go test serves: it `import`s the card module,
defines a **stub `hass`** (a fake `states` map for the climate entity + companion,
and `callApi`/`callService` spies), instantiates `<ha-lua-enhanced-climate-card>`,
and sets `.hass`. The chromedp test then:
- asserts the rendered DOM (status, target, schedule rows) matches the stub state;
- clicks the temp stepper / a boost preset / saves a schedule and asserts the
  right `callService('climate', …)` / `callApi('…events/ha_lua_command', …)`
  payload was captured;
- updates the stub `hass` and asserts the card reconciles (optimism-free);
- sets stub `hass.language = "hu"` and asserts the translated DOM (mirrors the
  existing `LocalizesHungarian` Ingress test).

Pure helpers (§10.5) also get plain assertions in the harness. Skips cleanly when
no browser is present, like the existing browser tests.

## 11. Testing (daemon & example)

**Go — generic transport:** `SetState`/`RemoveState` against `httptest`
(method/path/auth/body, 200/201/404, ctx-cancel); `config_test` RestURL force +
derive; capturing-stub tests for `ha.set_state`/`ha.on_command` (non-raising).

**Go — shared pure lib (`lib/control.lua`, like `TestSchedulePureLib`):**
`desired` priority, `is_manual` tolerance, `should_write` gating, `clamp_bounds`,
and `window_open` any-open/all-closed — table-driven, no `Runner`. The migrated
`thermostat.lua` keeps its existing tests green (proves the refactor is
behavior-preserving).

**Go — new example (via `Runner`, mirroring `TestThermostatAPI`):**
1. `configure` creates an enhanced climate, starts control, publishes the
   companion; re-`configure` same config is a no-op; changed config updates.
2. `remove` stops control and `remove_state`s the companion.
3. Control loop drives a store-defined climate: schedule/override priority,
   write-gating on mode/window/changed, manual-hold detection.
4. Window pause/restore from dynamically-bound sensors, including the
   multi-sensor case (any-open pauses, only all-closed restores).
5. Command round-trips: `settings` (bounds-rejected out of range), `schedule`
   round-trip, `override` start/cancel — each re-publishes the companion.

The existing `thermostat.lua` tests are untouched; card UI tests are in §10.8.
Green under `-race` + `make check`.

## 12. Milestones / commits (each compiles + `make test`)

**M1 — generic transport (reusable daemon, ships in the binary):** ✅ done
1. `ha: add core REST client for entity set/remove` (the REST base is derived
   from the WS URL inside `ha.New`, so the planned separate
   `config: derive/force the REST URL` commit is unnecessary — §4.1)
2. `lua: add ha.set_state / ha.remove_state / ha.on_command + lib/card.lua`

**M2 — shared lib + the new example (additive, examples-only):**
4. `examples: extract pure control helpers to lib/control.lua` (migrate
   `thermostat.lua` onto them — behavior-preserving, existing tests stay green —
   plus `lib/control` unit tests)
5. `examples: enhanced_climate registry + configure/remove handlers`
6. `examples: enhanced_climate control loop via lib/control + bounds`
7. `examples: enhanced_climate multi-sensor window cooperation`
8. `examples: enhanced_climate companion-sensor publishing`

**M3 — removal UI:** 9. `examples: enhanced_climate Ingress removal page`

**M4 — card UI (§10):**
10. `config: materialize bundled cards into /config/www/ha-lua` (small Go
    change: `CardsDir` forced in add-on mode, best-effort `Materialize`)
11. `cards: enhanced-climate-card render + climate-native controls + i18n`
12. `cards: enhanced-climate-card boost + override-temp + schedule editor`
13. `cards: enhanced-climate-card config editor (getConfigElement)`
14. `cards: enhanced-climate-card chromedp harness test` (§10.8, incl. a hu
    localization assertion)

**M5 — docs/release:** DOCS (`ha.set_state`/`on_command`, the card config + the
dashboard-resource install, the entity model, restart caveat, **the card needs
an admin HA user** to fire `ha_lua_command`, and a recommended recorder
`exclude` for `sensor.ha_lua_enhanced_climate_*`) + CHANGELOG + **minor** version
bump (new APIs + new example + bundled card + a behavior-preserving
`thermostat.lua` refactor; nothing breaks).

> M1 + the M4 materialization ship in the binary; M1 is reusable by any script.
> M2–M3 are examples-only (reference tree, never loaded — AI.state); a user copies
> `enhanced_climate.lua` into `/config/ha-lua/scripts/` to use it. `thermostat.lua`
> is touched only by the mechanical `lib/control.lua` migration (commit 4) —
> behavior and tests unchanged. The card JS (M4 commits 11–14) is the only
> frontend deliverable.

## 13. Open items

- **Multiple cards, one climate entity** — idempotent `configure` makes this
  safe. `presets` is cosmetic (last writer wins). `window_sensors` is **not**
  cosmetic — a window binding changes control behavior — so a card lists *every*
  relevant sensor and the daemon's any-open/all-closed reduction (§7.1) does the
  right thing; the last `configure` still wins the stored list, so the Ingress
  page (§8) is the authoritative place to reconcile a climate watched by several
  cards.
- **Admin requirement** — firing `ha_lua_command` via `events/` needs an admin HA
  user; documented (§12 M5), not worked around. Non-admin users keep the
  climate-native controls (`callService`), which do not require it.
- **Example name** — `enhanced_climate.lua` chosen for consistency with the
  concept/entity/card naming; rename if preferred before M2.
