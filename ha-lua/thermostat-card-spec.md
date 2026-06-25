# Thermostat dashboard card — Lua-side integration spec

Status: proposed (2026-06-25)
Scope: **Lua script changes only** (`examples/thermostat.lua`, possibly a
small shared lib). No Go daemon changes required. The Lovelace custom card
(JS) is a separate deliverable and is out of scope here.

## 1. Goal

Let a native Lovelace **custom card** on an HA dashboard read and control the
thermostat scheduler, in parallel with the existing Ingress web UI. The card
must show live zone state (mode, target, override/manual, schedule) and let the
user nudge the override temp, start/cancel a timed override, edit the schedule,
and reorder zones — the same surface the HTTP API already exposes.

This is **additive**. The Ingress UI served by `internal/web` stays exactly as
is; the card is a second front-end onto the same controller state.

## 2. Why events, not entities (decision)

A dashboard card can only talk to a backend through Home Assistant core — it
cannot fetch the add-on's Ingress URL directly (the ingress session token isn't
available to card JS; that path is the documented Frigate-card pain point). The
two HA-brokered channels are:

1. **Entity state + `hass.callService`** — the most "native" option, but ha-lua
   today only *ingests* entity state (`state.Tracker`); it has no path to
   *publish* an entity. Adding one means new Go surface (REST `POST /api/states`
   or an MQTT-discovery/companion-integration). That is explicitly **not** a
   lua-side change, and it drags in a second component to version.

2. **Custom events** — `ha.on_event` (inbound) and `ha.fire_event` (outbound)
   **already exist** and already round-trip over the one WS connection. A card
   fires an HA event to command; subscribes via `hass.connection.subscribeEvents`
   to receive state. Zero daemon code.

We choose **(2)**. It matches the user's "lua-side changes" framing, needs no
new Go, and keeps the whole feature inside the example script — the Linus
choice (least new machinery for the result).

Entity publishing is **deferred**, not rejected forever: if the user later wants
the schedule to appear in entity lists / history / `restore_state`, revisit it
as a separate daemon milestone (§9).

## 3. Existing primitives this builds on (no changes needed)

| Primitive | Location | Role here |
|-----------|----------|-----------|
| `ha.on_event(type, fn)` | `internal/lua/api_ha.go:292` | script subscribes to the card's command event |
| `EventTypes()` → `OnLoaded` → `client.AddEventType` → `subscribe_events` | runner/supervisor/`ha.client` | makes the subscription live on (re)load and across reconnects |
| `ha.fire_event(type, data)` | `internal/lua/api_ha.go`, wired in `cmd/ha-lua/main.go:185` | script pushes the state snapshot to the card (fire-and-forget over WS) |
| `full_state()` / the JSON the HTTP `GET /api/state` returns | `examples/thermostat.lua` | the exact payload to fire; reuse verbatim |
| internal mutators behind the HTTP handlers (set override-temp, set schedule, start/cancel override, set order) | `examples/thermostat.lua` | reuse so event handlers and HTTP handlers share one code path |

## 4. Event contract

Two event types. Underscored, namespaced to avoid collisions on the HA bus.

### 4.1 Inbound — `ha_lua_thermostat_cmd` (card → script)

One event type, dispatched on an `action` field, so the script registers a
single `ha.on_event` and the card opens a single channel. Payload:

```jsonc
{ "action": "get" }                                   // request a snapshot
{ "action": "override",    "zone": "living", "minutes": 30 }
{ "action": "override",    "zone": "living", "cancel": true }
{ "action": "settings",    "zone": "living", "override_temp": 21.3 }
{ "action": "schedule",    "zone": "living", "schedule": [ /* entries */ ] }
{ "action": "order",       "order": ["living","bedroom","kitchen"] }
```

- Field names and value shapes **mirror the existing HTTP API** (override_temp,
  schedule entry shape, order array) so both front-ends share validation and
  mutator code. Do not invent a second schema.
- Unknown `action` → ignore (log via `ha.on_exception` path is overkill; a
  no-op is fine — the bus is shared and other producers may exist).

### 4.2 Outbound — `ha_lua_thermostat_state` (script → card)

The full controller snapshot, **identical to `GET /api/state`** (zones with
mode, target, min/max, override, manual, schedule today-strip, order, etc.),
plus a tiny envelope:

```jsonc
{
  "rev":   1719331200,        // monotonic-ish stamp (time.now unix or a counter)
  "state": { /* exactly full_state() */ }
}
```

`rev` lets the card drop out-of-order deliveries (events can interleave under
rapid edits). The card always renders the highest `rev` it has seen.

## 5. Semantics

### 5.1 Reconciliation, not acks (matches the existing UI)

Events are fire-and-forget; there is no per-command response code. The handler
for **every** command does: validate → mutate iff valid → **always call
`publish_state()`**. A rejected command (e.g. override_temp out of device
bounds) simply doesn't change state, so the re-published snapshot makes the card
snap back to truth. This is the same optimism-free model the HTTP UI already
uses (AI.state: "no optimistic input.value write … re-render from server
state"). No separate error event in v1.

### 5.2 Cold start

A freshly loaded card has no data and `subscribeEvents` only sees *future*
events. So on connect the card fires `{action:"get"}`; the script replies with a
full `ha_lua_thermostat_state`. (Card-side; noted here so the contract is
complete.)

### 5.3 Live updates

`publish_state()` is also called whenever the controller mutates state from any
source:
- the 1-minute `ha.every` tick after it recomputes desired/publishes,
- the existing **HTTP handlers** (so editing in the Ingress UI updates the card
  live, and vice-versa),
- the window-cooperation handoff (heating_windows restoring desired).

Put the `publish_state()` call in the shared mutators, not in each entry point,
so every path that changes state emits exactly once.

### 5.4 Broadcast / multi-tab

HA custom events are global broadcasts: every subscribed card/tab receives every
`ha_lua_thermostat_state`, and a `get` from one tab refreshes all. That is
harmless (idempotent render) and desirable. No per-client targeting in v1.

## 6. `thermostat.lua` changes

1. **Extract shared mutators** (if not already factored): each HTTP handler
   currently parses a body then performs a state change. Ensure the change part
   is a plain function — `set_override_temp(zone, t)`, `set_schedule(zone, s)`,
   `start_override(zone, mins)`, `cancel_override(zone)`, `set_order(list)` —
   returning `ok, err`. The HTTP handlers call these; so will the event handler.
   (Reuse existing names; this list is descriptive, not prescriptive.)

2. **`publish_state()`**: build the same table `full_state()` produces, wrap it
   as `{rev = <stamp>, state = <full_state()>}`, and
   `ha.fire_event("ha_lua_thermostat_state", payload)`. `rev` = `time.now():unix()`
   is enough (a monotonic counter in `store` is overkill; ties are broken by the
   card keeping the latest received).

3. **Call `publish_state()`** at the end of every mutator (one call site each)
   and once after the initial publish at load, mirroring the existing
   "initial publish runs once at load" behavior.

4. **Register the command handler** at load:
   ```lua
   ha.on_event("ha_lua_thermostat_cmd", function(event)
     local cmd = event.data or {}
     if cmd.action == "get" then
       -- fall through to publish below
     elseif cmd.action == "override" then
       if cmd.cancel then cancel_override(cmd.zone)
       else start_override(cmd.zone, cmd.minutes) end
     elseif cmd.action == "settings" then
       set_override_temp(cmd.zone, cmd.override_temp)
     elseif cmd.action == "schedule" then
       set_schedule(cmd.zone, cmd.schedule)
     elseif cmd.action == "order" then
       set_order(cmd.order)
     end
     publish_state()   -- always reconcile, success or reject
   end)
   ```
   (Validation lives inside the mutators, exactly as the HTTP path relies on.)

5. **Bounds reuse**: schedule/override-temp validation must call the same
   `temp_bounds(zone)` device-range logic the HTTP API uses (AI.state 2.3.0), so
   the card can't push a setpoint HA will silently drop.

No change to `lib/zones.lua`, `lib/schedule.lua` (pure), `internal/web`, the
router, or any Go package.

## 7. Security

Any HA user/automation that can fire an event can drive the thermostat — same
trust level as calling a service. HA already gates the WS/REST event API behind
auth, and the add-on is admin-installed, so this is acceptable. The event names
are not secret; validation in the mutators is the real guard (already present
for the HTTP path). Do **not** accept entity ids or temps the mutators wouldn't
accept over HTTP.

## 8. Testing (Go, `internal/lua`)

Drive the real script through a `Runner` the way `TestThermostatAPI` already
does, but exercise the event path:

1. **`TestThermostatCmdGetPublishes`** — inject a capturing `FireEvent` stub into
   the runner; deliver an `ha_lua_thermostat_cmd {action:"get"}` via the runner's
   `SendHAEvent`; assert one `ha_lua_thermostat_state` fired whose `state`
   matches `GET /api/state` for the same seed. (Confirms cold-start round-trip
   and payload parity.)
2. **`TestThermostatCmdOverrideTemp`** — fire `settings {override_temp:X}`;
   assert the published snapshot reflects X, and an out-of-bounds X is rejected
   (snapshot unchanged) — reuses the max_temp=30 seed from `TestThermostatAPI`.
3. **`TestThermostatCmdScheduleRoundTrip`** — fire a `schedule` command, then a
   `get`; assert the schedule round-trips (mirror of the existing
   ScheduleSaveRoundTrip but over events).
4. **`TestThermostatHTTPMutationPublishes`** — drive a `PUT /api/settings` via
   the Router and assert a `ha_lua_thermostat_state` is *also* fired, proving the
   two front-ends stay in sync (§5.3).

The capturing `FireEvent` stub is the only new test scaffolding; `SendHAEvent`,
the Router test harness, and the seed helpers already exist. No browser test
(the card JS is out of scope). Everything must pass `make test` under `-race`.

## 9. Out of scope / deferred

- **The Lovelace card JS** (LitElement module, `subscribeEvents`,
  `callApi('POST','events/ha_lua_thermostat_cmd', …)`, dashboard resource) — a
  separate deliverable.
- **Entity publishing** (§2 option 1) — only if the user wants the schedule in
  entity lists/history. Needs a Go change (REST `set_state` or MQTT discovery);
  revisit as its own milestone, not part of this spec.
- **Per-client targeting / request correlation** — broadcast is sufficient (§5.4).
- **A dedicated error event** — reconciliation covers v1 (§5.1).

## 10. Commits (each compiles + `make test`)

This is example-tree + tests, so commits land in `examples/` and
`internal/lua/*_test.go`:

1. `examples: factor thermostat state mutators for reuse` — extract the
   plain mutator funcs the HTTP handlers call (if not already split). No
   behavior change.
2. `examples: publish thermostat state as an HA event` — add `publish_state()`
   and call it from every mutator + initial load. Test:
   `TestThermostatHTTPMutationPublishes`.
3. `examples: accept thermostat commands over HA events` — register the
   `ha_lua_thermostat_cmd` handler. Tests: `TestThermostatCmdGetPublishes`,
   `…OverrideTemp`, `…ScheduleRoundTrip`.
4. `docs: document the thermostat card event contract` — DOCS.md section + this
   spec's event table; CHANGELOG entry. (Examples-only ⇒ minor bump when
   released; the card JS ships alongside or after.)

> Note: examples are reference-only and never loaded (AI.state). The user's live
> `/config/ha-lua/scripts/thermostat.lua` must be updated separately for the
> card to work against their real deployment.
