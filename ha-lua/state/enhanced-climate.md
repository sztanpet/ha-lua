# State: enhanced climate (enhanced-climate-spec.md)

Working state for the REST/command transport, `examples/enhanced_climate.lua`
and the Lovelace card (`cards/enhanced-climate-card.js`). Spec:
`enhanced-climate-spec.md`. The overshoot learner inside the controller has its
own file, `overshoot.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE and live** — it is the controller that actually runs the
user's heating. Card VERSION **0.3.42** (v4.15.0).

## How it reaches a user
- The card is embedded and Materialized to
  `/config/www/ha-lua/enhanced-climate-card.js` on boot: a card fix needs a new
  image AND a restart. The user adds it once as a dashboard resource.
- The script is a hand-copied example: the live
  `/config/ha-lua/scripts/enhanced_climate.lua` and its libs must be re-copied
  for any script change, then the add-on restarted (`lib/` is not watched).
- Firing `ha_lua_command` needs an **admin** HA user.

## Controller decisions and why
- **Window pause writes frost (15, clamped).** A self-contained controller
  cannot just skip the write: the device would coast at its last setpoint.
- `written` (commanded) vs `desired` (requested): manual detection compares
  against `written`, and `written` moves only in heat mode. Recording it in
  every mode latched our own frost as a dial hold after heating went off and
  on.
- A boost keeps the manual hold underneath; the `restore:` snapshot of the
  device setpoint is used only when nothing controls the climate after the
  boost, because on a controlled climate that snapshot can be our own frost.
- A boost ends via its own `ha.after`, not the minute tick (the card showed a
  countdown frozen at 00:00); the tick is the backstop after a restart.
- Configure always republishes the companion, even when the config is
  unchanged: the card sends configure precisely when it cannot see the
  companion (HA drops these non-integration entities on restart).
- The companion is deduped by payload with a 5 min heartbeat, so an unchanged
  sensor is not a recorder row a minute but still self-heals after an HA
  restart.
- `radiator_entity` is part of `configHash` (the daemon needs it for the
  learner); before the learner it was display-only and deliberately kept out.

## Card lessons (each cost a release or several)
- **Configure is fire-once from MODULE scope, keyed by `entity|configHash`.**
  HA recreates card elements constantly, so per-instance guards cannot work; a
  reconcile-against-server-state-on-a-timer design stormed forever; a
  per-entity key ping-pongs between the saved card and the editor preview. See
  the `ha-card-module-level-guards` memory.
- **The editor preview must never provision.** HA sets `hass` BEFORE
  `preview`, so the check is deferred to a microtask.
- **Commands ride `hass.callWS({type: "fire_event"})`, never `callApi`.** A REST
  fetch to the private-IP HA host tripped Firefox 152's Local Network Access
  check, which tore down the whole frontend websocket. The card makes zero REST
  calls.
- `set hass` re-renders only when the climate, its companion, the radiator
  sensor or the language changed (reference compare; HA replaces state objects).
- The steppers echo taps locally and debounce writes (see the
  `local-echo-for-controls` memory).
- HA removed `ha-entities-picker`; the editor uses the singular
  `ha-entity-picker`, and `window_sensors` stays a list of 0 or 1.
- Pending spinner: cleared when the companion's `last_updated` changes, 6 s
  fallback. It says "working", never "done".
- Opt-in tracing: `localStorage["ha-lua-debug"] = "1"`.

## Card harness notes
- The card is a CLASSIC script (`node --check` works). Pure helpers are exposed
  as `HaLuaEnhancedClimateCard.pure` for tests.
- `internal/lua/enhanced_climate_card_test.go` serves it with a stub hass
  (`callApi`/`callService`/`callWS` spies). It runs here: node and chromium
  are installed.
- Rendering uses requestAnimationFrame, which never fires under
  `chromium --dump-dom`: shim rAF for DOM dumps (screenshots are fine).
- `<ha-icon>` is undefined outside HA, so mode buttons render as empty pills in
  any harness. Not a card bug.
- Preset pill corners use `:first/last-of-type`, not `-child`: the pending
  spinner rides inside `.presets`.
