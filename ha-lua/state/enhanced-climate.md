# State: enhanced climate (enhanced-climate-spec.md)

Working state for the enhanced-climate track: the reusable REST/command
transport, the `enhanced_climate.lua` example, and the bundled Lovelace card
(`cards/enhanced-climate-card.js`). Spec: `enhanced-climate-spec.md`. Global
decisions live in `../AI.state`.

Status: **track COMPLETE, released v2.7.0; card iterated through v2.8.11.**
Current card VERSION **0.3.28**.

## v2.8.11 (card 0.3.28) — held badge on the title row
- 0.3.28 (4bac9a6, shipped v2.8.11): held badge moved from a `.header` flex
  sibling (it floated between the title and subtitle lines) into a new
  `.title-row` flex row: right edge via `margin-left: auto`, vertically
  centered on the title line. With a wrapped two-line title the badge centers
  across both lines — acceptable, verified visually.
- Verified in headless Chromium with a fake-hass harness (scratchpad):
  screenshot + DOM dump. Gotcha for future harnesses: the card renders via
  requestAnimationFrame, which never fires under `chromium --dump-dom`
  (screenshot mode is fine) — shim rAF to setTimeout for DOM assertions.

## v2.8.9–v2.8.10 (cards 0.3.25–0.3.27) — render skip + status-line polish
- 0.3.25 (9bcf713, shipped v2.8.9): `set hass` re-renders ONLY when the climate
  entity, its companion, or hass.language changed (reference compare — HA
  replaces state objects immutably). Previously every state change anywhere in
  the install rebuilt the whole shadow DOM. Marker-based harness test.
- 0.3.26 (6162fa0): window open/closed moved from its own row onto the subtitle
  (`19.5° | heating | window open`, warning colour when open; .row/.label CSS
  removed). formatClock now takes hass and honours hass.locale.time_format
  ("24"→h23, "12"→h12, else language default) — en profiles on 24h no longer
  get AM/PM. i18n keys window.open/window.closed now carry the full phrase;
  bare "window" key removed.
- 0.3.27 (8ae7a59, shipped v2.8.10): held badge has a title tooltip explaining
  the manual hold (dial change kept until next scheduled transition).
- Card rewrite considered (2026-07-01) and REJECTED — see
  `state/code-review.md` for the reasoning and rejected refactors.

## How the card reaches a user's HA
The card is embedded in the binary (`cards/` package) and materialized to
`/config/www/ha-lua/enhanced-climate-card.js` on every boot (served at
`/local/ha-lua/enhanced-climate-card.js`). So ANY card fix only reaches a user
once a new image ships AND the add-on restarts to re-materialize it. The user
adds it once as a dashboard resource (type: module). The card fires
`ha_lua_command` events — which requires an **admin** HA user.

## v2.8.5 (card 0.3.21) — configure is fire-once, reconciler DELETED
- The reconcile model (0.3.19) was fundamentally wrong and kept regressing. User
  confirmed window.sensors for konyha_furdo is ALWAYS empty (so _companionConfigured
  SHOULD have matched) yet the card retried every 15s anyway — and 2.8.4's
  republish-on-every-configure turned that retry into a state_changed storm that
  dropped the HA WS and constantly reloaded the page.
- FIX (1a94bb6): deleted _reconcileConfig + _companionConfigured + pendingConfigure
  + CONFIGURE_RETRY_MS. Configure is now strictly FIRE-ONCE: module-level
  `sentConfigures` Map (climate_entity -> configHash); _maybeConfigure (called from
  setConfig + set hass) sends once per distinct config, NO companion check, NO
  retry. A storm is now structurally impossible regardless of companion state or
  how often HA rebuilds the card. Daemon keeps companion ownership (tick +
  republish-on-configure from 2.8.4). Test rewritten: 50 hass updates => exactly 1
  configure; a real config change => exactly 1 more.
- Daemon force-republish-on-configure (2.8.4, ed33763) KEPT — it's safe and useful
  now that the card fires once: each page load => 1 configure => companion refreshed
  (self-heals an HA-dropped companion on load).
- LESSON: don't make a card reconcile against server state on a timer; tell the
  server once and let the server own its own published entities.

## v2.8.4 (card 0.3.20) — terminate reconcile loop + fix editor picker
- After 0.3.19 the storm became a bounded ~15s retry (the reconcile backoff): the
  card never reached its fixed point because `_companionConfigured` was permanently
  false for konyha_furdo (empty config). Root cause: the companion was ABSENT (HA
  drops these non-integration entities on restart) and the daemon's configure
  handler NO-OPED on unchanged config (config_equal true) — so it never
  republished, and my publish dedup cache stopped the 1-min tick from healing it
  until the 5-min heartbeat. Deadlock: card asks → daemon ignores → card retries.
- Daemon fix (ed33763): configure ALWAYS (re)publishes the companion — clears
  `published[e]` then apply_climate so publish_companion writes even when
  unchanged; registry write + log still gated on a real change. Test
  TestEnhancedClimateConfigureRepublishesCompanion (repeat identical configure =>
  +1 companion write). Card stops as soon as the companion reappears.
- Editor fix (4bdf9cf): window-sensor field used `ha-entities-picker` (plural) —
  a frontend internal HA DROPPED, so it rendered as an unknown element (no picker,
  no label, couldn't configure window sensors). Switched to single
  `ha-entity-picker` per the user's call ("use a single element picker, it will be
  fine"). window_sensors stays a LIST (0 or 1 entry) so control/companion code is
  untouched; label now singular (en "Window sensor", hu "Ablakérzékelő").
- NOTE: `ha-entities-picker` being gone is the kind of HA-internal churn the card
  header comment warns about; `ha-entity-picker` (singular) still ships.

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

## Card 0.3.22 (post-2.8.5) — editor configure-storm, real root cause

2.8.5's fire-once Map (entity->configHash) was STILL wrong. The user hit a
storm when EDITING a card: right after picking a window sensor the page
hammered the ha-lua endpoint and HA broke (Save button unclickable).

Real root cause (verified against HA frontend hui-card._loadElement):
- While the edit dialog is open, TWO card elements for the SAME climate entity
  are mounted: the saved dashboard card (config A) behind the dialog, and the
  editor PREVIEW (config B, e.g. with the new window sensor) inside it. HA
  pushes hass to both on every state change.
- The guard Map stored only the LAST hash per entity, so A and B each saw the
  other's hash, decided their config had changed, and re-sent configure. Each
  configure -> daemon republishes companion -> another hass push -> another
  double-send: infinite ping-pong. Keying per-entity was the bug.
- HA also sets element.preview=true on the preview and RECREATES it on every
  keystroke; a throwaway preview was writing real config to the daemon.

Fixes (two commits):
- 512b849 `cards: dedupe configure per (entity, config), not entity`: Map ->
  Set keyed "entity|hash". Every distinct (entity,config) sends exactly once
  regardless of how many cards are mounted. Regression test
  TestEnhancedClimateCardConfigureTwoCardsNoStorm (two cards/two configs + 50
  interleaved hass pushes => exactly 2 configures, not a storm).
- 18e7a50 `cards: never provision the daemon from the editor preview`: skip
  when this.preview. HA assigns hass BEFORE preview in _loadElement, so the
  check can't run synchronously in the hass setter — deferred to a microtask
  via _scheduleConfigure (drains within the same task, so fire-once guarantees
  hold). Regression test TestEnhancedClimateCardPreviewNoConfigure.

LESSON: the editor preview is a throwaway element HA recreates constantly and
flags with .preview — it must never mutate server state; and a per-entity guard
is unsafe whenever two cards can target one entity (always true while editing).
Released as **v2.8.6** (2026-06-26, tag on 0e7cd05; changelog dc35a0a).

## Card 0.3.23 (v2.8.7) — feedback spinner, diagnostics, version banner

UX: pressing an override action (preset/custom/cancel/override-temp) had no
feedback during the daemon round-trip (HA event -> daemon -> companion
republish -> push back), so a tap felt dead. Added a pending state (c3ef521):
_command() fires the command, sets this._pending, and forces a synchronous
_renderNow() (a click pushes no hass, so otherwise nothing would show); the
override buttons render disabled with a `.spinner` alongside. Cleared in
set hass via _clearPendingIfChanged() when the companion stamp changes
(companion.last_updated bumps on every republish — the genuine confirmation),
with a PENDING_TIMEOUT_MS=6000 fallback so a dropped command can't stick it.
Stays optimism-free: the spinner says "working", not "done". Test
...PendingSpinner (click -> spinner + disabled; companion push -> cleared).

Diagnostics + versioning (0bf0bdc): VERSION banner had drifted to 0.3.21 (2.8.6
shipped card changes without bumping it), so a cached browser was
indistinguishable from current code. Bumped to 0.3.23; MUST bump every card
change. Added opt-in dbg() gated by localStorage["ha-lua-debug"]=="1" tracing
connect/disconnect/_command/fireCommand(ws.connected)/configure + a module-level
pagehide stack trace.

OPEN ISSUE (not fixed in 2.8.7): user (Firefox 152) reports "The connection to
wss://…/api/websocket was interrupted while the page was loading" + "Subscription
not found" after each card interaction. RULED OUT: the card (only callApi REST +
callService; no <form>/<a href>/location; all buttons type=button) and the daemon
(override/settings handlers only call climate.set_temperature + publish_companion
set_state — nothing that reloads a page or restarts HA). The console also logs
Firefox 152 "Local Network Access detected" on the very same ha_lua_command fetch
(target 192.168.1.139:443), and the site uses split-horizon DNS (public hostname
-> private IP). Leading hypothesis: Firefox 152's new Local Network Access feature
disrupts the local wss connection whenever a local-network fetch happens — i.e.
browser/env, not our code. Next diagnostics: (a) does a NATIVE HA card action
repro the same lines? (b) enable ha-lua-debug and check whether `pagehide` fires
(real document reload) vs. just a HA WS reconnect.

## Card 0.3.24 (v2.8.8) — commands over the websocket (Firefox 152 LNA fix)

SOLVED the open WS-drop issue from 0.3.23. The opt-in dbg() tracing pinned it:
- pagehide NEVER fired -> the page does not reload; "interrupted while the page
  was loading" is just how HA's socket.js phrases a dropped wss. The
  disconnect/connect afterwards is HA rebuilding the card after its socket
  reconnects (normal).
- the wss:// connection died ~8ms after each `fireCommand` fetch, simultaneously
  with Firefox 152's "Local Network Access detected" log on that fetch
  (target 192.168.1.139:443). HA's own cards (hui-history-graph-card) also errored
  with failed results / "Subscription not found" — the WHOLE frontend socket
  dropped, not just ours.

Root cause: hass.callApi fires the event via a REST fetch, which opens a NEW
connection to HA's private-IP host. Firefox 152's new Local Network Access check
on that fetch tears down the live wss:// connection as a side effect. callService
(mode/temp) never triggered it because it already rides the websocket.

Fix (66e5c13): fire over the existing socket with
hass.callWS({ type: "fire_event", event_type: "ha_lua_command", event_data })
(HA core's websocket_api exposes fire_event). Same event-bus delivery -> daemon
on_command unchanged. The card now makes ZERO REST calls. Falls back to callApi
only if the frontend predates callWS. Harness gained a callWS spy (window.__calls.ws);
tests assert the command rides the ws spy and NOT the REST spy. Card VERSION 0.3.24.

LESSON: a custom card that does a REST fetch to a private-IP HA host can drop the
whole frontend websocket on Firefox 152 (Local Network Access). Prefer callWS /
callService (they reuse the open socket) over callApi for anything that fires per
user action.

## Card 0.3.29 (v2.8.12) — override row on one line

The override fieldset wrapped on typical masonry columns (~360px content):
four standalone duration buttons + the 160px override-temp stepper ≈ 430px.
Rework (73a12f2):
- duration presets fuse into ONE segmented pill (same shared-border trick as
  the 0.3.17 setpoint stepper): `.presets .btn` loses its radius except the
  outer corners via :first/last-of-type (NOT :first/last-child — the pending
  spinner span rides inside .presets, tests select `.presets .spinner`).
- pill segments are `flex: 1 1 0; min-width: 0` and .presets is `flex: 1`,
  so the pill splits whatever row width the stepper leaves — this is what
  actually guarantees the one-line fit (fixed shaving alone left it ~2px
  over at 396px). On narrow cards (<~310px) flex-wrap drops the pill to its
  own full-width line, no clipping (verified at 276px).
- stepper first in .override-controls: the temp control stays put when an
  override starts (it used to jump from last to… not present).
- active state is now just countdown + Stop; the "overriding to X°" text and
  its en/hu translation keys are GONE — the stepper beside it shows the temp.
- `.stepper .value` narrowed 72px → 60px (padding 6px 4px) — shared with the
  main setpoint stepper, both fine ("21.5" fits centered).

Verified with a chromedp screenshot harness (scratchpad, stub hass like the
card test): idle + active on one line at 396px, pending = disabled pill +
spinner, 276px wraps gracefully. Note the mode buttons render as empty pills
in any stub harness — ha-icon is undefined outside the HA frontend; harness
artifact, not a card bug.
