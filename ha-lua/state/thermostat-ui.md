# State: thermostat UI + heating examples (thermostat-ui-spec.md)

Working state for the thermostat web-UI track and the heating example scripts
(thermostat, heating_windows, valve_watch) that share `examples/lib/zones.lua`.
Spec: `thermostat-ui-spec.md`. Global decisions live in `../AI.state`.

Status: **track COMPLETE (milestones 1–7).** Example scripts received many
post-release tweaks (below). Released across 1.1.0 → 2.6.0.

NOTE on vocabulary: the 2026-06-23 rename (28a15b9) renamed UI "boost"→"override"
and dial-detected "override"→"manual". Pre-2026-06-23 notes below still say
"boost"/"comfort"; read them as today's "override"/"override_temp". The old
"override" (dial) is today's "manual".

## VOCAB RENAME boost->override, override->manual (2026-06-23, 28a15b9)
- The UI "boost" (timed 10/30/60-min hold) never boosted -- it is a timed
  manual override; the UI already said "Temporary override". Renamed to
  "override" everywhere. That collided with the existing dial-change override
  (detected setpoint change held to next schedule transition), so THAT is now
  "manual". desired() priority: override (UI, timed) > manual (dial) > schedule.
- Renamed: store keys boost:->override:, override:->manual:,
  comfort:->override_temp:; HTTP /api/boost->/api/override (+/cancel),
  comfort_temp->override_temp; CSS .boost*/.boosting->.override*/.overriding;
  i18n boost/boosting_to/stop_boost/edit_comfort -> override/overriding_to/
  stop_override/edit_override_temp; lua active_boost->active_override,
  active_override->active_manual, comfort()->override_temp(); test ids
  overrideTemp->manualTemp, TestThermostat{ManualOverrideDetected->ManualHold
  Detected, BoostSuppressesOverride->OverrideSuppressesManual}, UI Boost
  Flow->OverrideFlow, ComfortStepper->OverrideTempStepper.
- Examples-only + tests + DOCS. Historical specs/CHANGELOG keep old words. NOT
  released. Store-key rename orphans live values (revert to default once).
- Released as 2.6.0 (2026-06-23, tag v2.6.0): minor bump for this rename
  (example HTTP API + stored keys changed; daemon untouched).

## Drag-to-reorder thermostat cards (2026-06-23, examples/thermostat.{lua,html})
- REQUEST: make the thermostat cards re-orderable and persist the order on the
  backend so other browsers/users see the same arrangement.
- BACKEND (f3c8436): one fixed store KV key "zone_order" = array of zone ids.
  ordered_zones() filters it to zones that still exist and appends any omitted
  ones alphabetically, so the served order always covers exactly the current
  zone set however stale the stored value is -> UI renders straight from it.
  full_state() gained an `order` field; new PUT /api/order validates ids,
  collapses dupes, accepts a partial list. GET stays alphabetical until set.
- FRONTEND (a74a241): six-dot grip in each card head. render() now lays cards
  out in state.order (fallback sorted keys). Drag is POINTER EVENTS, not HTML5
  DnD, so it works on touch+mouse; touch-action:none on the grip stops a
  touch-drag scrolling the page. onDragMove swaps DOM positions when the dragged
  card's centre crosses a neighbour's, nudging startY by the neighbour height so
  the translate stays continuous (no jump). endDrag PUTs the DOM order, holds
  the zone busy until the PUT resolves (no mid-drag poll rebuild), and on a
  failed PUT re-renders from lastState -> reverts the move. New i18n key
  "reorder"; card carries data-zone.
- TESTS: TestThermostatOrderAPI (Go, no browser) drives GET/PUT order, rejects
  unknown zones, checks partial-list append, and the persist-across-readers
  round-trip. TestThermostatUIReorderPersists (chromedp) does a REAL pointer
  drag via CDP mouse input and asserts the swap survives a fresh navigation.
- Released as 2.5.0 (2026-06-23, tag v2.5.0).

## Comfort setpoint: always-visible input (2026-06-23, examples/thermostat.html)
- REQUEST: tap-to-reveal setpoint (84fadb1) was too slow; make it a permanent
  number field so an exact target can be typed without first tapping.
- The .val span + editComfort() swap is gone; the stepper now renders a
  persistent <input class=val-input type=number inputmode=decimal>. Focus marks
  the zone busy (suppresses the 5s poll), blur/Enter commit, Escape reverts.
- +/- nudge the COMMITTED setpoint (commitComfort(comfort_temp ± 0.1)); no
  optimistic input.value write -- an earlier optimistic version let the browser
  test's settled() pass pre-response and re-rendered PUT replies out of order
  under -race, leaving a stale number. lastSent guard dedupes the no-op blur PUT.
- nudge buttons use onmousedown preventDefault so clicking them keeps field
  focus (no blur-commit mid-click).
- Test: thermostat_ui_test.go firstCardComfort reads .val-input .value.

## Thermostat boost vs device max_temp (2026-06-23, examples/thermostat.*)
- BUG REPORT: boost did nothing. Root cause: boost sets the setpoint to the
  zone's comfort temp; comfort stepper/validation allowed 5..35 but the user's
  TRVs cap at max_temp=30. HA SILENTLY rejects a set_temperature above max_temp
  (CallService was fire-and-forget over the WS), so a 31-32° comfort temp made
  boost a no-op with nothing logged.
- FIX: thermostat.lua temp_bounds(zone) reads entity min_temp/max_temp (fallback
  5..35 pre-seed); /api/settings validates comfort against it; /api/state now
  publishes min_temp/max_temp per zone. thermostat.html: stepper + tap-to-edit
  number input both clamp to [min,max].
- ROOT CAUSE NOW FIXED (daemon change): CallService was fire-and-forget. Added
  Client.SendCommandWaitResult -- registers a waiter by msg id in a `pending`
  map, readLoop/getStates route "result" frames to it, success:false -> error
  carrying HA's message. 10s liveness timeout + drain-on-disconnect so a script
  goroutine never hangs. main wires CallService through it; the Lua binding
  already raises call_service errors to on_exception. FireEvent left fire-and-
  forget. Tested in ha/client_test.go TestSendCommandWaitResult. NOTE:
  call_service is now SYNCHRONOUS (waits for HA's ack, ~ms) -- a behavior change
  for all scripts, but only adds error-raising on already-failing calls.
- Schedule editor also bounded: schedule.validate gained optional (lo, hi) args
  (default 5..35); PUT /api/schedule passes temp_bounds(zone); html editor temp
  inputs clamp via zoneBounds(zone).
- Released as 2.3.0 (2026-06-23): comfort+schedule device-range bounds, tap-to-
  edit target, call_service result checking.
- Status badge (2026-06-23, 2.4.0): card head shows "on" when mode==heat, or
  "heating" while hvac_action=="heating", never the raw "heat" word. zone_state
  exposes hvac_action; UI statusLabel() in thermostat.html. TestThermostatUI
  StatusLabel covers heating / idle->on / absent->on. Examples-only.

## Browser UI tests — override/stepper/i18n/schedule (2026-06-21)
- Extended internal/lua/thermostat_ui_test.go (chromedp) with DOM-driven tests,
  each its own commit, all reusing serveThermostatUI/newBrowserCtx and skipping
  cleanly when no browser is found.
- OverrideFlow: click a preset → POST /api/override → card body swaps to the
  countdown → cancel → preset row returns.
- OverrideTempStepper: +/− → PUT /api/settings; the quantisation lives in the
  page (setComfort), so only a browser test covers it.
- LocalizesHungarian: ?lang=hu rewrites the shipped <h1> and translates zone
  names + legend via t(). GOTCHA: the legend is CSS text-transform:uppercase, so
  chromedp.Text (innerText) returns the upper-cased form — read textContent via
  Evaluate instead.
- ScheduleSaveRoundTrip: seeded zones ship NO schedule (testZonesLua has no
  schedule field), so the editor opens with 0 rows. Add one entry → save (PUT) →
  reopen (GET) → regroups 5 weekday rows to one Mon–Fri entry. Must
  Poll(editorAnimationsDone) before clicking add/save — the open animation
  max-height-clips them and clicks miss.
- NotControlledCard + LanguagePicker: seed one zone "off" → its card shows the
  .muted "not controlled" notice while the two heat cards keep controls
  (serveThermostatUISeed). Picker: switch #lang to hu, persists to localStorage
  and reloads WITHOUT ?lang=; fire the change from setTimeout(...,0) so the
  Evaluate returns before location.assign destroys the JS context.
- COMFORT STEPPER IS NOW 0.1° (57a1a1d, user request): setComfort steps ±0.1 and
  rounds to the 0.1 grid; server already accepts any 5..35 so no backend change.
  Released as 2.1.0 (2026-06-21).
- ScheduleTempTenths (77aca25): the comfort stepper quantises to 0.5° but the
  editor temp input is step="0.1"; type 21.3 into a new entry, save, reopen, and
  assert it reloads unrounded.

## Valve watchdog (2026-06-21, examples/valve_watch.lua)
- New script catches a seized/dead radiator valve (the user's valves fail
  ~every 2 years): the thermostat keeps calling for heat but no hot water
  reaches the radiator, so the radiator sensor stays at room temp.
- lib/zones.lua gained a per-zone `radiator` sensor entity id (placeholder ids,
  user edits to match HA). thermostat/heating_windows ignore the new field.
- Alert rule: calling-for-heat AND radiator rise < MIN_RISE (3°C) over the
  WARMUP (15m) window AND NOT already-hot (rad-room < HOT_MARGIN 8°C). The hot
  guard prevents a false alarm when the radiator was already hot. Heat demand
  prefers climate `hvac_action == "heating"`, falls back to current < target-0.1.
- Notify target NOTIFY_TARGET default "notify.pixel_9a", parsed "<domain>.<svc>"
  and sent via ha.call_service with title+message.
- STATELESS REFACTOR (per user): dropped the {since,base,notified} per-zone
  store record. Baseline is now read from ha.get_history (oldest radiator
  reading in the window); demand-duration is confirmed from the climate
  entity's OWN history (every row since `since` shows demand active; empty
  window => NOT confirmed). The detection signal is identical, only its source
  moved. Bonus: a restart mid-episode no longer mis-captures the baseline.
- The ONLY remaining state is a per-zone "alerted:<zone>" boolean (cleared when
  demand stops) so a dead valve alerts once per episode, not once a minute.
- `since` for get_history is now a time VALUE, not a string (2026-06-21 refactor).
  GetHistory takes time.Time and renders the bound itself: UTC, truncated to
  whole seconds, no zone suffix -- a lexical prefix of any same-second
  changed_at. Killed the old foot gun where a hand-formatted local-tz string
  mis-sorted lexically and dropped rows. Script just passes now:add(-WARMUP);
  the utc_since helper is gone. t:utc() stays as a general re-zone helper.

## Thermostat UI — Milestones 2–7 (2026-06-17)
- M2 Packaging (commit ee7667a): config.yaml gained ingress/ingress_port/
  panel_icon/panel_title, a ports: 8100/tcp mapping, and http_port as an
  option+schema. RESOLVED the §5.5 contradiction (ingress_port is "manifest
  only" but Go must bind it): IngressPort is forced to 8099 in ADD-ON MODE ONLY
  inside config.load(); dev mode leaves it 0 and binds just the LAN port. main
  starts a second web.Start on the ingress port sharing the one Router. The Go
  const ingressPort=8099 MUST stay equal to config.yaml ingress_port.
- M3 Controller (commit c6aad7d): scripts/thermostat.lua + scripts/lib/zones.lua
  + scripts/lib/schedule.lua. desired() = boost > override > schedule. 1-min
  ha.every tick publishes global:thermostat:desired:<zone> and writes the
  climate entity only when mode==heat AND no window open AND value changed
  (>0.05). Manual-override detection: a climate target differing from published
  desired (tolerance >0.1) starts an override until the next transition; boost
  and open/UNSEEDED windows suppress it. Override store field is named
  "expires" NOT "until" (Lua keyword). Initial publish runs once at load.
- schedule.lua is PURE (no ha/store/time) so it is Go-unit-testable: resolve()
  returns (active_temp, now_index 0-based or -1 carryover, minutes_to_next with
  7-day fwd scan); weekday converted lua_dow=(go_weekday+6)%7 (0=Mon). validate()
  bounds temp 5..35 and HH:MM. Tested in scripts_test.go TestSchedulePureLib.
- M4 Window cooperation (commit 7ccb236): heating_windows.lua rewritten to use
  lib/zones and restore global desired on close (dropped save-the-setpoint).
  TestWindowHandoffRestoresPublishedDesired runs the real script in a runner.
- M5 HTTP API (commit 9634706): GET /api/state, POST /api/boost,
  POST /api/boost/cancel (longest-prefix beats /api/boost), PUT /api/settings,
  GET+PUT /api/schedule. Server-side validation → 400. TestThermostatAPI loads
  the real script with libs+scheduler+Router and drives it.
- M6 UI (commit c62cdbb): GET / serves one self-contained HTML page (vanilla
  JS/CSS). Boost hero, per-zone comfort stepper, today strip, per-zone 7-day
  editor. RELATIVE fetch URLs (./api/...) for ingress+LAN. Polls 5s, local 1s
  countdown, polling suppressed while an editor is open.
- M7 Docs: DOCS.md Web UIs + Thermostat sections, http_port option;
  CHANGELOG 1.1.0; config.yaml version → 1.1.0.
- Empty Lua tables encode as {} (object), so today=[] arrives as {} — the UI
  guards with Array.isArray. luaMarshal already passes Deterministic(true).
- INGRESS NOT YET VERIFIED against a live Supervisor: depends on HA stripping
  the /api/hassio_ingress/<token> prefix before forwarding (so the router sees
  /api/state). Confirm on first real deploy; if false, every route 404s under
  ingress.
- TIMEZONE FIX (post-review): main now sets time.Local = ResolveLocation(
  cfg.timezone) so scripts' time.now() (drives the thermostat schedule via
  ha.every) agrees with the scheduler's ha.at. Before this, a non-UTC user on a
  UTC container would have schedules fire at the wrong wall-clock time. Boost/
  override DURATIONS were always fine (absolute instants).
- Override engine tests added (TestThermostatManualOverrideDetected,
  TestThermostatBoostSuppressesOverride) — the latter uses a 2nd zone as a FIFO
  barrier to make the negative (boost suppresses override) deterministic.

## Thermostat UI — Milestone 1: HTTP server core (2026-06-15)
- New Lua-facing HTTP server so a script can serve a UI. ha.serve(method,
  prefix, handler); handler returns status[, body[, headers]].
- internal/lua/router.go: Router (http.Handler). Table is (method,prefix)->
  scriptID, resolved through the Registry at request time (immediate 404 after
  stop; stale entry self-heals because api.routes is the authoritative lookup).
  Longest-prefix match. Body capped at 1 MiB.
- Request path is SEPARATE from the lossy event fan-out: Runner.reqCh is
  unbuffered and NEVER closed (sends can only block, never panic); reply chan
  is buffered cap 1 (run loop's reply never blocks even after client timeout).
  Single round-trip deadline (default 5s) → 503. The 503 bounds the client
  wait, not handler execution.
- Route lifecycle (spec §3.1a): Supervisor.afterLoad registers routes holding
  s.mu across the handle-identity check + Router.Register, fully serialized
  with StopScript's Router.Unregister (also under s.mu) — no dangling mapping
  on reload. Verified by TestRouterReloadReRegisters.
- callHandler reads (status, body, headers) defensively: PCall pads to NRet=3,
  garbage/short returns → 200, never a panic. Errors → on_exception + 500.
  Refactored luaErrParts shared with callProtected.
- internal/web/server.go: web.Start(ctx, addr, router) (mirrors debug.Start;
  no-op if addr empty). config.HTTPPort (http_port, 0=disabled); wired in main;
  config.dev.yaml sets 8100.
- Tests in internal/lua/router_test.go: round-trip, request-field echo,
  longest-prefix, 404 (unknown + wrong method), handler-error 500, garbage/
  no-return defaults, busy 503 (send timeout), reload re-registration.
