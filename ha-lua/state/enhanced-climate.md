# State: enhanced climate (enhanced-climate-spec.md)

Working state for the enhanced-climate track: the reusable REST/command
transport, the `enhanced_climate.lua` example, and the bundled Lovelace card
(`cards/enhanced-climate-card.js`). Spec: `enhanced-climate-spec.md`. Global
decisions live in `../AI.state`.

Status: **track COMPLETE, released v2.7.0; card iterated through v2.7.4.**
Current card VERSION **0.3.8**.

## How the card reaches a user's HA
The card is embedded in the binary (`cards/` package) and materialized to
`/config/www/ha-lua/enhanced-climate-card.js` on every boot (served at
`/local/ha-lua/enhanced-climate-card.js`). So ANY card fix only reaches a user
once a new image ships AND the add-on restarts to re-materialize it. The user
adds it once as a dashboard resource (type: module). The card fires
`ha_lua_command` events — which requires an **admin** HA user.

## v2.8.3 (card 0.3.19) — configure storm REAL fix
- 0.3.18's instance-level reconcile did NOT stop the storm (user confirmed still
  storming on 0.3.18 / add-on 2.8.2). Root cause it missed: HA recreates the card
  element faster than the daemon's companion publish round-trips back into
  hass.states, so each FRESH instance (with `_sentConfigHash === undefined` and a
  companion that hasn't caught up) re-fired configure before being destroyed. A
  per-instance guard provably cannot help — the recreation race is the whole bug.
- Rework: configure is now RECONCILIATION. Module-level `pendingConfigure` Map
  (climate_entity -> {hash, at}) survives element recreation; `_reconcileConfig`
  (called from set hass AND setConfig) sends configure only when the companion
  doesn't already match window_sensors/presets, single-flight with CONFIGURE_RETRY_MS
  (15s) backoff. Steady already-configured climate => ZERO POSTs. Removed
  `_maybeConfigure` + `_sentConfigHash`. Daemon owns the registry (SQLite, echoed
  via companion) — the card converges it, never eagerly pushes.
- Test TestEnhancedClimateCardConfigureNoStorm: 50 hass updates => <=1 configure
  (mismatch) and ==0 (config matches companion). Commit 45f6c55.

## v2.8.2 (card 0.3.18) — HA-freeze fix
- **CRITICAL: card configure storm.** _maybeConfigure fired `configure` from set
  hass guarded only by an in-instance config hash. HA recreates the card element
  freely (masonry/sections, every reconnect), so each fresh instance re-sent
  configure → daemon re-published the companion → new hass state → card recreated
  → configure again. Self-sustaining loop flooded POST /api/events/ha_lua_command
  until the frontend WS dropped (code 3 "Connection lost") and the UI froze
  (unclickable, console spammed with uncaught {success:false} rejections).
  Fix: reconcile against the companion the daemon already published
  (_companionConfigured compares window.sensors + presets); only send configure on
  real first-setup / config change. Also wrapped callApi/callService in
  Promise.resolve(...).catch(() => {}) so transient failures don't spam the
  console. Commit e44bc5a.
- **companion publish dedup.** publish_companion (examples/enhanced_climate.lua)
  rewrote every companion every minute even unchanged — a no-op state_changed +
  recorder row per companion per minute (the "refreshed companion" debug spam the
  user saw). Now caches JSON of the last sent payload per climate and skips the
  write when unchanged; PUBLISH_HEARTBEAT (5m) still forces a refresh so a sensor
  self-heals after an HA restart (published states are NOT integration-backed and
  Seed does not prune the mirror on reconnect, so get_state can't tell us HA
  dropped it). remove_climate clears the cache. Test
  TestEnhancedClimateCompanionDedupsUnchanged (re-fires identical window event →
  no write; window open → writes). Commit fe57c8f.

## v2.8.1 (card 0.3.17)
- setpoint stepper fused into one pill: dropped `.stepper` gap, ± buttons share
  the input's `--card-background-color`, internal borders removed (minus loses
  right border, input loses both side borders, plus loses left border), only the
  two outer corners rounded (12px). `.value` padding 4px→14px, width 64→72px.
  Styles BOTH steppers (manual target + override-temp) via shared _stepperControl.
- `.climate-controls` now `justify-content: space-between` so the HVAC mode icons
  right-align when sharing the stepper's row; flex-wrap applies justify per line,
  so once they wrap to their own line they fall back left.
- commits 79b0ee9 (pill) + 9fc78e3 (mode align, carries the 0.3.16→0.3.17 bump).

## v2.8.0 (card 0.3.16)
- override + schedule sections are now <fieldset> with <legend> as the title
  (legend rides the border → no separate heading row, saves vertical space).
- override mirrors thermostat.html: duration buttons render "10m" (was "+10m"),
  durations + the bare override-temp stepper share one wrapping .override-controls
  row inside the fieldset; an active override shows countdown + "overriding to X°"
  (new i18n overriding_to) + cancel. i18n override_for renamed back to override.
- window-sensor picker fix: ha-entities-picker was wrapped in a <label> (invalid;
  swallowed its add/remove clicks so selections never stuck) — now set .label and
  append directly like the working climate picker.
- removed dead _labelledStepper + .group-head/.head-title/.override-head CSS.
- harness: preset label "10m", hu anchor on the override <legend> ("Felülbírálás").

## Unreleased (example script)
- **UNRELEASED on main** (examples/enhanced_climate.lua, 2178b17): the script
  was silent except the on_exception error file. Added info-level logging of
  every HA interaction — set_temperature writes (only fire on change via the
  should_write gate), each card command (configure/override/schedule/settings/
  remove), manual-change detection, window-driven re-applies, load-time summary.
  Per-minute companion refresh is debug; first create is info; a FAILED publish
  now warns (was swallowed by non-raising set_state). REMINDER: examples are
  reference-only — the user's live /config/ha-lua/scripts/enhanced_climate.lua
  must be re-copied for this to take effect.

## Release log (card)
- **v2.7.7**, card 0.3.15 — every temperature field (target stepper, override-temp
  stepper, schedule editor) steps by the device's target_temp_step, default 0.1
  (was: override-temp hardcoded 0.5, editor fixed 0.1, only target honoured the
  device step). One tempStep computed in _renderNow, threaded through
  _renderEnhanced / _renderScheduleGroup / _openEditor (_editorStep). Harness
  asserts the override-temp stepper inherits the device step.
- **v2.7.6** (tag on 465bf68), card 0.3.13 → 0.3.14:
  - 0.3.13 button refactor — all buttons share one `.btn` style with modifiers
    (.icon mode buttons, .step ± glyphs, .active mode-colour fill, .primary Save,
    .ghost Add, .link/.danger the row ✕); the five old per-button styles are
    gone. `.group-head` is layout-only (uppercase moved to `.head-title`), so the
    schedule Edit button matches the rest and isn't uppercased. Empty schedule =
    single heading line with "no schedule set" as the tooltip. Stepper input
    matches the 44px/12px button metrics.
  - 0.3.14 JS simplification — entriesFromSchedule derives groups from DAY_GROUPS
    (no hardcoded day arrays; round-trip verified); makeTranslator one ?? chain;
    fireCommand/callClimate use spread; _renderMode builds the icon via h();
    _stepper renamed _labelledStepper (vs bare _stepperControl).
  All test hooks (.step/.mode-btn/.presets/.edit-schedule/.editor .save)
  preserved across both.
- **v2.7.5** (tag on 3bc05a4), card 0.3.9 → 0.3.12 — iterative card-layout polish
  (0.3.12: dropped the stray ° span that sat between the setpoint input and the +
  button):
  - 0.3.9: mode name is the button title (tooltip) + aria-label only, NOT a
    visible label (reverted the 0.3.8 visible label per user).
  - 0.3.10: target stepper + mode buttons share one wrapping row (labels
    dropped, stepper keeps aria-label, modes keep title); current mode/status
    moved beside the current temperature in the subtitle (current temp · status,
    thin divider) — the top-right status badge is gone; empty schedule renders a
    compact "—" with "no schedule set" as a tooltip. _stepper split into
    _stepperControl + labelled-row wrapper.
  - 0.3.11: override duration buttons inline with the heading, renamed
    "Override for:" (i18n override -> override_for); override/custom/cancel
    buttons restyled to the shared mode-button look (rounded rects via merged
    selectors).
  Harness anchors: status -> .subtitle .status; hu localization -> the
  override-temp .row .label ("Felülbírálás cél"); mode label -> title attribute.
- **v2.7.4** (674d002): native-look polish, card 0.3.8 — default grid span 12
  cols (was 6, still resizable); HVAC mode = rounded flex buttons with the mode
  name labelled under each icon (not bare circles); current temperature moved
  into the card title as a subtitle (own .current row dropped); title uses HA's
  exact .card-header CSS (--ha-card-header-* vars, --ha-font-size-2xl,
  --ha-font-weight-normal, -0.012em). Harness asserts the visible mode label.
- **v2.7.3** (8f215e5), card 0.3.7:
  - 0.3.6: getGridOptions returned columns:"full" which pinned the card
    full-width and overrode the user's layout options; now columns 6 /
    min_columns 3 / rows auto (resizable). Harness asserts columns!="full".
  - 0.3.7: dropped the redundant .enhanced border-top divider; override duration
    buttons always show (fallback DEFAULT_PRESETS 10/30/60 when card configures
    none); added a custom-duration "…" button that window.prompt()s for minutes
    (daemon already accepts 1..1440). Harness asserts default presets + custom.
- **v2.7.2** (b9660f5), card 0.3.5:
  - 0.3.2 getGridOptions: kill the sections-dashboard "does not fully support
    resizing" warning (was only getCardSize).
  - 0.3.3: stepper numeric input height (was shorter than the ± buttons).
  - 0.3.4: boost->override rename in the CARD UI (the 28a15b9 vocab rename had
    hit thermostat.html but not the later-written card; daemon command was
    already "override"). Renamed labels/i18n (en+hu)/CSS/_renderBoost.
  - 0.3.5: mode as icon buttons not a <select> (like HA's own card); override +
    schedule in separate bordered groups; schedule shows today's periods inline
    (read-only, active highlighted) via pure todayPeriods helper, edit still
    opens the 7-day editor.
- **v2.7.1** (1d09a32), card 0.3.1: fix the config editor. HA sets an element's
  hass before setConfig, so the editor's _render ran with this._config undefined
  and threw "can't access property climate_entity" as a "Configuration error"
  the instant the card was added. Fixed 521e2be (guard editor _render on
  !this._config too).
- **v2.7.0** (tag on edc744f, NOT the bare release commit 4f3ed78, per user
  request — folded in two example cleanups: dbb7590 dedupe friendly_name +
  apply_all in enhanced_climate, edc744f fix helper count in lib/control). M1–M5
  done. config.yaml at the tag reads 2.7.0 so the GHCR workflow builds right.

RESOLVED (2026-06-25): a "Setting up…" report (companion sensor never published)
was user error, not a code bug — the card works once the example is deployed
correctly (script copied into /config/ha-lua/scripts/, admin user, set_state ok).

## Card structure (for future work)
- `cards/enhanced-climate-card.js` is a CLASSIC script (no import/export, so
  `node --check` works directly). Tooling here: node v18 + /usr/bin/chromium
  present, so the chromedp harness RUNS (not skip).
- Pure helpers exposed as `HaLuaEnhancedClimateCard.pure` for the harness:
  slugOf, companionId, statusLabel, clampNumber, configHash, formatClock,
  remainingSeconds, formatCountdown, entriesFromSchedule, scheduleFromEntries,
  todayPeriods, makeTranslator, MESSAGES, DAY_GROUPS.
- Render is optimism-free: `_render()` (hass-driven) early-returns while
  `_fieldFocused` (an input is focused) or `_editorOpen` (schedule editor);
  `_renderNow()` forces a render for local interactions. The 1s override
  countdown is a text-only setInterval (no re-render).
- MESSAGES has en+hu; hu status.on is the English word "on" per the Ingress-UI
  decision. MODE_ICONS maps hvac mode -> mdi icon (via <ha-icon>); DEFAULT_PRESETS
  = [10,30,60].
- Config editor (`...-card-editor`) uses HA's undocumented ha-entity-picker /
  ha-entities-picker — works only inside a live HA frontend, NOT in the harness.
- Harness test internal/lua/enhanced_climate_card_test.go: serves the card from
  the cards embed FS + a stub hass with callApi/callService spies; asserts
  render, grid not pinned full, mode buttons (not select) + label + set_hvac_mode,
  override preset command, default presets + custom button, target-stepper
  service, optimism-free, reconcile, hu "Cél" label, inline today strip,
  schedule-save command.

## Build history (M1–M5)
- **M1 generic transport** (e4e9020, 0a95c26, a2c029e): internal/ha/rest.go adds
  Client.SetState (POST /states/{id}, 201->created/200->updated) + RemoveState
  (DELETE, 404=already-gone), ~10s http.Client, shares WS token. deriveRESTURL
  turns the WS URL into the REST base (ws->http/wss->https, strip /websocket,
  ensure /api) — handles BOTH ws://supervisor/core/websocket -> .../core/api and
  ws://host:8123/api/websocket -> .../api. ha.New stays 2-arg. Lua bindings
  ha.set_state/remove_state are NON-RAISING (value|nil,err, like http/fs) so the
  per-minute publish can't spam on_exception during an outage; ha.on_command
  (handler) filters data.script==script id, calls handler(action, data.data);
  ha.script_id exposed. lib/card.lua: card.new{kind=...} -> dot-call table
  (card.on/publish/remove), publish -> sensor.ha_lua_<kind>_<slug> stamping
  ha_lua_script marker; kind defaults to ha.script_id.
- **M2 control loop** (3e11155, f580b28, 747aae4, f1e274e, ee9d2e9): extract pure
  helpers to lib/control.lua + migrate thermostat.lua behavior-preserving;
  enhanced_climate registry + configure/remove handlers; control loop via
  lib/control + bounds clamp + manual detection; multi-sensor window
  cooperation; companion sensor publishing. Per-climate store keys
  schedule:/override:/manual:/override_temp:/desired:; 1-min tick iterates the
  registry; apply_climate clamps to device bounds + should_write gate; wildcard
  climate.* on_state_change does manual-hold detection (one handler covers
  runtime-added climates).
  - Registry: global key "enhanced_climate:registry" (map climate_entity ->
    {climate_entity, window_sensors, presets}); configure idempotent via
    config_equal.
  - Window pause design: a SELF-CONTAINED controller can't just skip the write
    (device coasts at last setpoint), so it writes a FROST_TEMP (15, clamped to
    device min) while any window is open and restores the desired when all
    close. Manual detection is suppressed while a window is open/unknown (else
    our own frost write looks like a dial change).
- **M3 removal Ingress page** (674fcea): examples/enhanced_climate.html served at
  GET /, GET /api/list + POST /api/remove, shared remove_climate(). An enhanced
  climate outlives any card, so removal is explicit (a deleted card can't send
  remove).
- **M4 card JS** (c16ad75 materialize cards; e3fd064 render + climate-native
  controls + i18n; 1fbdc27 override presets/countdown/cancel + override-temp
  stepper + window + 7-day schedule editor, preceded by a9205ca surfacing
  override_temp on the companion; b5bd0e4 config editor getConfigElement;
  4292ab6 chromedp harness test).
- **M5 docs/release** (5afab4e DOCS, 8d48168 CHANGELOG, 4f3ed78 version->2.7.0):
  DOCS.md covers ha.set_state/remove_state/on_command, the card config +
  dashboard-resource install, the entity model, the restart caveat, the
  admin-user requirement, and a recommended recorder exclude for
  sensor.ha_lua_enhanced_climate_*.
