# Thermostat dashboard card — HA integration spec

Status: proposed (2026-06-25, rev 2 — entity publishing folded in)
Scope: a new **general daemon capability** (`ha.set_state` / `ha.remove_state`)
plus **`examples/thermostat.lua`** changes that use it. The Lovelace custom
card (JS) is a separate deliverable and is out of scope here.

## 1. Goal

Let a native Lovelace **custom card** on an HA dashboard read and control the
thermostat scheduler, in parallel with the existing Ingress web UI. The card
shows live per-zone state (mode, target, override/manual, schedule) and lets the
user nudge the override temp, start/cancel a timed override, edit the schedule,
and reorder zones — the same surface the HTTP API already exposes.

This is **additive**. The Ingress UI served by `internal/web` stays exactly as
is; the card is a second front-end onto the same controller state.

## 2. Channels and the entity decision

A dashboard card can only talk to a backend through Home Assistant core — it
cannot fetch the add-on's Ingress URL directly (the ingress session token isn't
available to card JS; that is the documented Frigate-card pain point). So both
directions go through HA. They use **different** mechanisms because HA core is
asymmetric:

### 2.1 Outbound (script → card): **publish HA entities** (the read channel)

The script publishes each zone as a real HA entity via the core REST API
(`POST /api/states/<entity_id>`, the documented "set state + attributes"
endpoint — 200 if the entity existed, 201 if new). The card reads
`this.hass.states['sensor.ha_lua_thermostat_living'].attributes` and re-renders
automatically on every `hass` update.

This is the **cleaner** channel than the fire-and-forget state event from rev 1,
and is now the chosen design:

- **Retained state, not a transient pulse.** A fresh card has the data
  immediately from `hass.states` — no cold-start `get` handshake, no missed
  events before the subscription opened.
- **Native reactivity.** HA pushes `hass` updates to the card on every state
  change for free; no `subscribeEvents` plumbing on the card side.
- **Shows up in HA proper.** Entities appear in the entity list, history,
  logbook, more-info dialogs, and can be used by other cards/automations — a
  real integration surface, not a private side channel.

The cost is a small **Go daemon change** (ha-lua only ingests state today; it
needs an outbound `set_state`). That capability — `ha.set_state` — is generic
and useful to **every** script, not just the thermostat, so it earns its place.

> **Rejected:** shipping a Python custom integration to register entities/
> services "properly." That is a second codebase in a second language deployed
> to `/config/custom_components` — strictly more machinery for the same result.
> REST `set_state` from the daemon we already run is the Linus choice.

### 2.2 Inbound (card → script): **custom event** (the command channel)

`set_state` can't *control* anything, and registering an HA **service** requires
an integration (rejected above). The remaining no-new-component inbound path is
a **custom event**, which ha-lua already handles via `ha.on_event`
(`internal/lua/api_ha.go:292`) — the runner subscribes through
`client.AddEventType` → `subscribe_events`, live on every (re)load and across
reconnects. So the card fires one event to command; **zero daemon change** on
this side.

## 3. New daemon capability: `ha.set_state` / `ha.remove_state`

A general binding, specced here because the card needs it; reusable everywhere.

### 3.1 `internal/ha` — REST client

`ha-lua` is WS-only today. Add a thin REST path to `ha.Client`:

- New fields: `restURL string`, `httpClient *http.Client` (≈10s timeout).
- `SetState(ctx, entityID, state string, attrs jsontext.Value) (created bool, err error)`
  → `POST {restURL}/states/{entityID}` with `Authorization: Bearer {token}`,
  `Content-Type: application/json`, body `{"state": state, "attributes": attrs}`.
  200/201 → ok (`created` from 201); any other status → error carrying the
  status code + a short body snippet.
- `RemoveState(ctx, entityID) error` → `DELETE {restURL}/states/{entityID}`
  (200 → ok, 404 → treat as already-gone/ok).
- The REST call is independent of the WS connection lifecycle; it shares only
  the token. (If HA is fully down, both fail — fine.)

Extend the constructor to take the REST base (`New(wsURL, restURL, token)`); the
test/call-site churn is mechanical.

### 3.2 `internal/config` — REST base URL

Mirror the existing add-on-forcing pattern in `config.load()`
(`config.go:117`), no new user option, no `config.yaml` schema change
(`homeassistant_api: true` is already granted — `config.yaml:14`):

- Add `HomeAssistant.RestURL` (`rest_url`), optional.
- **Add-on mode:** force `cfg.HomeAssistant.RestURL = "http://supervisor/core/api"`
  (the Supervisor core proxy), next to the existing `URL`/`Token`/`IngressPort`
  forcing.
- **Dev mode:** if `RestURL` is empty, derive from `URL` in `Defaults()`:
  scheme `ws→http` / `wss→https`, strip a trailing `/websocket` from the path →
  REST base (e.g. `ws://host:8123/api/websocket` → `http://host:8123/api`). The
  Supervisor `/core/websocket` shape isn't hit in dev, so the simple strip is
  enough; a user can still set `rest_url` explicitly.

### 3.3 `internal/lua` — bindings + wiring

- `Deps.SetState` / `Deps.RemoveState` funcs (like `CallService` / `FireEvent`,
  `supervisor.go:33`), wired in `main.go` from the `ha.Client` methods.
- Lua bindings in `api_ha.go`:
  - `ok, err = ha.set_state(entity_id, state, attributes)`
  - `ok, err = ha.remove_state(entity_id)`
- **Non-raising** (`value|nil, errmsg`), matching the `http`/`fs`/`json`
  convention — **not** `call_service`'s raise-to-`on_exception`. Rationale:
  publishing runs every tick; a transient HA outage would otherwise spam
  `on_exception` once per zone per minute. The script ignores the error and the
  next tick re-publishes. (`call_service` raises because a dropped command is a
  real, rare failure; a dropped state publish self-heals.)

## 4. Entity model (outbound)

One entity per zone, plus one index entity.

### 4.1 Per-zone — `sensor.ha_lua_thermostat_<zone_id>`

- **state**: current target setpoint (°C, numeric) when the zone is controlled;
  `"off"` when `mode != heat`.
- **attributes** (mirror the per-zone fields of `full_state()` / `GET /api/state`):
  `friendly_name` (zone name), `mode`, `hvac_action`, `current_temperature`,
  `target_temperature`, `override` (`{active, expires, temp}`), `manual`
  (`{active}`), `min_temp`, `max_temp`, `window_open`, `schedule` (the zone's
  7-day entries — small, well under HA's attribute size limit, so the card's
  editor can render straight from `hass.states`), `order` (index), `controlled`
  (bool). Plus `unit_of_measurement: "°C"`, `device_class: temperature`,
  `icon: mdi:thermostat`.

### 4.2 Index — `sensor.ha_lua_thermostat`

- **state**: zone count.
- **attributes**: `order` (array of zone ids), `zones` (array of the per-zone
  entity ids). Lets the card discover the zone set and order in a single read
  without scanning entity ids.

### 4.3 Lifecycle

- Publish all entities on **script load** (initial), in the existing **1-minute
  `ha.every` tick**, and at the end of every **mutator** (§5.2) so any change —
  card, Ingress UI, schedule transition, window handoff — pushes immediately.
- **Restart transience (the one real caveat):** REST-set states are *not*
  integration-backed, so an HA restart drops them. ha-lua re-publishes on its
  next tick (≤1 min) and on reconnect-triggered reload, so they reappear on
  their own. Acceptable for a dashboard; document it in DOCS.md.
- When a zone disappears from config, `ha.remove_state` its entity so a stale
  card entry doesn't linger (compare published set vs current zones on load).

## 5. Command contract & handlers (inbound)

### 5.1 `ha_lua_thermostat_cmd` (card → script)

One event type, dispatched on an `action` field (one `ha.on_event`, one card
channel). No `get` action — entities are retained, so the card never needs to
poll for a snapshot.

```jsonc
{ "action": "override", "zone": "living", "minutes": 30 }
{ "action": "override", "zone": "living", "cancel": true }
{ "action": "settings", "zone": "living", "override_temp": 21.3 }
{ "action": "schedule", "zone": "living", "schedule": [ /* entries */ ] }
{ "action": "order",    "order": ["living","bedroom","kitchen"] }
```

Field names/shapes **mirror the HTTP API** (override_temp, schedule entry shape,
order array) so both front-ends share validation + mutators. Unknown action →
no-op.

### 5.2 Reconciliation, not acks

Events are fire-and-forget; there is no per-command ack. Each handler does:
validate → mutate iff valid → **re-publish entities** (§4.3). A rejected command
(e.g. override_temp out of device bounds via the same `temp_bounds(zone)` check,
AI.state 2.3.0) simply doesn't change state, so the re-published entity makes the
card's `hass.states` snap back to truth. Same optimism-free model the Ingress UI
already uses. No separate error channel in v1.

## 6. `thermostat.lua` changes

1. **Shared mutators** (factor if not already): `set_override_temp(zone,t)`,
   `set_schedule(zone,s)`, `start_override(zone,mins)`, `cancel_override(zone)`,
   `set_order(list)`, each returning `ok, err`. HTTP handlers *and* the event
   handler call these. (Names descriptive; reuse what the handlers call today.)
2. **`publish_zone(zone)` / `publish_all()`**: build the per-zone table and the
   index table and `ha.set_state(...)` them. Log-and-continue on the returned
   error (do not raise).
3. **Call `publish_all()`** from: initial load, the 1-min tick (after it
   recomputes desired), and the end of each mutator.
4. **Register the command handler** for `ha_lua_thermostat_cmd` (§5.1),
   dispatching to the mutators, then `publish_all()` unconditionally.
5. **Bounds reuse**: validation goes through `temp_bounds(zone)` so the card
   can't push a setpoint HA silently drops.

No change to `lib/zones.lua`, `lib/schedule.lua` (pure), `internal/web`, or the
router. The `fire_event` outbound state event from rev 1 is **dropped** in favor
of entities.

## 7. Security

`homeassistant_api: true` already grants the supervisor token core REST access,
so no manifest change. Anyone who can fire an event or write a state via that API
can drive the thermostat — same trust level as `call_service`, already gated by
HA auth + admin-only add-on install. Validation in the mutators is the real
guard (present for the HTTP path); the event handler must not accept anything the
HTTP path wouldn't.

## 8. Testing

**Go — `internal/ha`:** `SetState`/`RemoveState` against an `httptest.Server`:
assert method, path (`/states/<id>`), `Authorization` header, JSON body; map
200→ok, 201→`created`, 4xx/5xx→error; context-cancel path. (No live HA.)

**Go — `internal/config`:** add-on mode forces `RestURL` to the supervisor proxy;
dev mode derives it from a `ws://…/api/websocket` URL; explicit `rest_url`
survives. Extend the existing `config_test.go` table.

**Go — `internal/lua`:** inject capturing `SetState`/`RemoveState` stubs into
`Deps`; bind-level test that `ha.set_state` returns `ok`/`err` per the stub and
is non-raising on stub error.

**Go — `internal/lua` (thermostat, via `Runner` like `TestThermostatAPI`):**
1. `TestThermostatPublishesEntities` — on load, capturing `SetState` receives one
   call per zone plus the index entity, attributes matching `GET /api/state`.
2. `TestThermostatCmdOverrideTemp` — fire `settings {override_temp:X}`; assert the
   re-published entity reflects X, and an out-of-bounds X is rejected (entity
   unchanged) — reuses the max_temp=30 seed.
3. `TestThermostatCmdScheduleRoundTrip` — fire a `schedule` command; assert the
   published `schedule` attribute round-trips.
4. `TestThermostatHTTPMutationPublishes` — drive `PUT /api/settings` via the
   Router; assert a `set_state` is also emitted (two front-ends stay in sync).

No browser test (card JS out of scope). All green under `-race` + `make check`.

## 9. Out of scope / deferred

- **The Lovelace card JS** (LitElement; reads `hass.states['sensor.ha_lua_thermostat*']`;
  commands via `hass.callApi('POST','events/ha_lua_thermostat_cmd', …)`;
  dashboard resource) — separate deliverable.
- **Per-client targeting / request correlation** — retained entities + broadcast
  command are sufficient.
- **A dedicated error event** — reconciliation covers v1 (§5.2).
- **Integration-backed entities surviving HA restart without a re-publish** —
  would need a real custom integration (§2.1, rejected); the ≤1-min re-publish
  is good enough.

## 10. Commits (each compiles + `make test`)

Daemon changes (general, reusable) first, then the example + docs:

1. `ha: add core REST client for entity set/remove` — `Client.SetState`/
   `RemoveState` + `httptest` tests.
2. `config: derive and force the core REST API base URL` — `RestURL` field,
   add-on force + dev derive + `config_test.go` cases.
3. `lua: add ha.set_state / ha.remove_state bindings` — `Deps.SetState`/
   `RemoveState`, `api_ha.go` bindings, `main.go` wiring, capturing-stub tests.
4. `examples: publish thermostat zones as HA entities` — `publish_all()` from
   load + tick + mutators; remove zones via `remove_state`. Tests:
   `TestThermostatPublishesEntities`, `TestThermostatHTTPMutationPublishes`.
5. `examples: accept thermostat commands over HA events` — `ha_lua_thermostat_cmd`
   handler. Tests: `…CmdOverrideTemp`, `…CmdScheduleRoundTrip`.
6. `docs: document set_state + the thermostat card contract` — DOCS.md
   (`ha.set_state`/`remove_state`, the entity model, command event, restart
   caveat) + CHANGELOG. `ha.set_state` is a new Lua API ⇒ **minor** bump.

> Commits 1–3 are real daemon changes shipped in the binary. 4–5 are
> examples-only (reference tree, never loaded — AI.state); the user's live
> `/config/ha-lua/scripts/thermostat.lua` must be updated separately for the
> card to work against their deployment.
