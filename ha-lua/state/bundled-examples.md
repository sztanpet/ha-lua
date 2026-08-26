# State: bundled reference examples (load-examples-spec.md)

Working state for the read-only bundled examples tree. Spec:
`load-examples-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE.** Shipped in 2.2.0 (2026-06-22, tag v2.2.0). Examples keep
growing on top; newest: `ikea_dimmer.lua`, MQTT-driven (2026-08-26, v4.7.0).

## Bundled reference examples (2026-06-22)
- The repo's example/script tree doubled as the author's personal heating
  deployment (real konyha_* entity ids). Split the two concerns: the scripts
  are now a generic REFERENCE set, the author's real config lives only in their
  /config (recoverable from git history if needed).
- `git mv scripts/ -> examples/` (history preserved). Sanitized lib/zones.lua to
  placeholder ids (climate.living_room, ...). The ONLY code coupling was the
  test const repoScriptsDir ("../../scripts" -> "../../examples"); tests use an
  inline testZonesLua fixture, so renaming/sanitizing was ~zero test churn.
- Examples are REFERENCE-ONLY: never loaded or run. Rejected an earlier
  load_examples runnable-second-source design (Supervisor multi-source +
  precedence + watcher fallback) as needless complexity — running examples
  against entities a stranger lacks is useless, and "run my own scripts from the
  bundle" is a deployment concern (git), not the add-on's job.
- New `examples/embed.go` (package bundled): `//go:embed *.lua lib/*.lua *.html`
  + `Materialize(destDir)` (overwrites every file each boot, writes a generated
  README pointing at ../scripts/). embed.go excluded by the patterns.
- config: `ExamplesDir` forced to /config/ha-lua/examples in add-on mode (like
  LogDir/IngressPort); NO user option, NO schema change. Dev leaves it empty =>
  no materialization (don't write into a /config that may not exist).
- main: Materialize runs BEFORE the blocking HA seed wait, so the reference
  appears even when HA is unreachable; best-effort (warn, never fatal).
- Supervisor/watcher/Registry/Scheduler/Router/store UNTOUCHED. Only scripts/
  is loaded and hot-watched, exactly as before.

NOTE: the cards/ embed package (enhanced-climate-card.js → /config/www/ha-lua)
follows this same Materialize pattern — see `enhanced-climate.md`.

## mirrored_switches latency follow-up (2026-07-07)
- User benchmarked mirrored_switches.lua against the equivalent built-in HA
  automation and found ha-lua empirically slower. Root cause: the default
  100 ms event batch window (internal/lua/runner.go batchWindow), NOT the
  architecture — the inherent floor (two WS hops + WAL write before dispatch)
  is a few ms. The example never called ha.immediate_events().
- 46a0f6b examples: mirrored_switches now calls ha.immediate_events() with a
  comment explaining the human-visible-lag case AND why batching is the
  default (real event loss without it).
- e1b57e7 docs: loud callouts everywhere a user would look — lua_api.md
  immediate_events blockquote ("first thing to check when a script feels
  slower than a built-in automation") + pointer from on_state_change,
  DOCS.md "feels slower?" note after the first-script walkthrough + API
  table row, README.md design-decisions entry on the batching trade-off.
- Deliberately NOT changed: the 100 ms default itself — batching was added
  because unbatched bursts REALLY dropped events (user-confirmed history,
  not hypothetical), so immediate delivery stays per-script opt-in — and
  the tracker-write-before-dispatch ordering in main.go
  (load-bearing: handlers read partner state via ha.get_state from the
  mirror, so persisting first is what makes the handler see fresh state).
- Round 2 (2026-07-07): with immediate_events the user still saw high
  VARIANCE vs built-in automations. Cause: OpenDB set no synchronous
  pragma → SQLite default FULL → one WAL fsync per state_changed commit,
  serialized on the single write connection, on the dispatch critical
  path (queues behind every other entity's fsync too). c09f35f sets
  synchronous=NORMAL (+ regression test asserting PRAGMA synchronous=1
  on both handles). Remaining known spike sources if variance persists:
  WAL autocheckpoint (~1000 pages, runs on the committing writer) and
  the hourly purge DELETE holding the write connection; both left alone
  until actually measured as a problem.
- Both rounds shipped in v3.0.1 (2026-07-07, tag on 4cdd2a1).

## battery_levels example (2026-08-06)
- New bundled example: `battery_levels.lua` + `battery_levels.html`, a
  **Batteries** tab (`ha.ui`) listing level / last change / rundown ETA,
  sorted so the first battery to die is on top. Commit 7eb5584 (script,
  page, tests), be24e8c (DOCS.md section). RELEASED in v4.1.0.
- The design decision worth remembering: the ETA cannot come from
  `ha.get_history`. State history is purged after `retention_days` (2 by
  default) and a battery forecast needs weeks. So the script keeps its OWN
  series in its KV store (`series:<entity_id>`), one sample per observed level
  change — a handful of rows a month per entity, capped at MAX_SAMPLES=120.
- Fit is least squares over the samples PLUS the current instant as a point.
  Appending "now" is what makes a battery that stopped moving decay towards a
  flat slope instead of forever predicting last month's rate.
  **SUPERSEDED 2026-08-09** — the fit and its guards are gone, see below.
- A rise > RECHARGE_JUMP (2 points) wipes the series: a charge or a swap makes
  every earlier sample worthless. **SUPERSEDED 2026-08-09** — now 10 points
  above the run's low, see below. Guards before any ETA is shown at all:
  >= 3 samples, >= 6 h span, >= 2% drop, negative slope. Otherwise the page
  says "measuring".
- Discovery is configuration-free: `device_class: battery` (state IS the
  percentage) plus any entity with a numeric `battery_level` attribute. The
  two are NOT interchangeable for "last changed" — a device_tracker's
  last_changed tracks the phone moving, not the battery — hence the
  `level_is_state` flag, and nil (not "just now") for a first sighting.
- `changed_at` takes the OLDER of HA's last_changed and our newest sample:
  HA's is exact but resets on every HA restart, ours is honest about age but
  up to one scan interval (15 min) late.
- Series cleanup is by "entity gone from the state mirror entirely"
  (`ha.get_state == nil`), never by "missing from this pass" — an unavailable
  device must not lose weeks of samples.
- Tests: `internal/lua/battery_levels_test.go` drives the real example through
  the Router — rundown math + urgency order, discovery/exclusion, recharge
  reset, and a chromedp browser test for the rendered rows and the
  client-side sort.

## battery_levels: per-battery ignore (2026-08-08)
- Commit `a734f9c`. RELEASED in **v4.3.0** (2026-08-08, tag on `4579cb4`).
- The static `IGNORE` table in the script is GONE, replaced by an `ignored`
  key in the script KV store toggled from the page (`POST /api/ignore`
  `{entity_id, ignored}`, replying with the whole rebuilt scan payload so the
  page never has to patch a row by hand).
- Semantics the user asked for and the tests pin: an ignored battery is **not
  hidden**. It keeps its row and its level, dims, and sorts last — in the
  daemon's urgency order (`by_urgency` checks `ignored` first) AND in every
  client-side sort mode (one trailing stable partition in `sorted()`).
- Ignoring means STOP TRACKING: it is never sampled, and `forget_removed` now
  also deletes the series of anything just ignored, so tracking it again
  starts from one fresh sample instead of a stale slope.
- `goto continue` is not available — gopher-lua is Lua 5.1. The scan loop
  branches with if/else instead.
- Tests: `TestBatteryLevelsIgnore` (still listed, last, no forecast, samples
  dropped, resume starts fresh, 404 unknown entity, 400 no entity_id) plus a
  click-through in the existing chromedp UI test.

## battery_levels: forecast from one step (2026-08-09)
- Commits `6c26427` (script + Lua-side tests), `36bd760` (page + chromedp).
  RELEASED in **v4.4.0** (2026-08-09, tag on `abf14e9`).
- The reported bug: nearly every row said "measuring". `MIN_SAMPLES = 3` meant
  **two observed level steps** (a sample is only written when the level moves),
  which on a 10%-granularity sensor is a 20% drop. The forecast was only ever
  visible on the batteries fast enough that nobody needed a forecast.
- Least squares is GONE, replaced by the secant across the whole observation
  window: `drop / (now - series[1].at)`. Why this is not a downgrade — with one
  sample per level step the endpoints carry the entire signal, and the two
  censoring errors cancel: `series[1]` is an *observation* (we caught the level
  part-way through its dwell, so the window is short by that remainder) and the
  dwell in progress at the current level is cut short by a remainder of its own.
  Over the window the drop counts as many whole steps as the elapsed time does,
  so the estimator is unbiased. It also degrades to ONE step, which is the point.
- `MIN_SAMPLES` and `MIN_DROP` are gone. Only `MIN_SPAN` survives, raised
  6 h → 24 h. That is the one knob that matters: with a single step, the window
  bounds how wrong the extrapolation can be, and 24 h caps the worst case at
  ~99 days instead of the ~25 days a 6 h window allowed. The estimate is
  pessimistic early and stretches on its own as the battery sits.
- New third case: a battery that has never stepped gets `eta_at_least`, a floor
  from `remaining / COARSEST_STEP * dwell` with `COARSEST_STEP = 10` (the
  coarsest granularity common hardware reports, so the floor stays true for
  fine-grained sensors too). It needs `dwell >= MIN_SPAN`, otherwise a battery
  that stepped a minute ago would be "dying within the hour".
- Sorting is three tiers (`urgency_rank`): measured ETA, then floor, then level.
  A floor NEVER outranks a measurement even when it is the smaller number — it
  is systematically pessimistic, so mixing the two by value would crowd the top
  of the page with ignorance. The chromedp test pins this.
- `EMPTY_LEVEL` 0 → **15** (user request). Countdown ends at the replace-it
  line, and a battery already below it reports `eta_seconds = 0` rather than
  nothing — the `remaining <= 0` branch, which did not exist when empty was 0.
- Page: `~4 mo` when `steps < 2`, `> 3 mo` for a floor (uncoloured — colouring a
  bound reads as a prediction), `measuring` only when there is neither.
- Rejected (asked, user said no): seeding the series from HA's own history.
  `recorder/statistics_during_period` keeps hourly long-term statistics forever
  for any `state_class: measurement` sensor, which would have given months of
  real data on first run. It needs `resultMsg` (internal/ha/types.go) to carry
  the result payload and a new Lua binding. Revisit only on request.

## battery_levels: recharge threshold is 10 from the low (2026-08-09)
- Commit `832f11e`. RELEASED in **v4.4.1** (2026-08-09, tag on `9c42c08`).
- `RECHARGE_JUMP` (> 2 points in one step) is GONE, replaced by
  `RECHARGE_RISE = 10` measured against the LOWEST sample in the series.
- The reported reason: real entities report levels that fluctuate by three or
  more points a day. At a 2-point bar those wiped their series on nearly every
  noisy uptick, so `series[1]` was never old enough for `MIN_SPAN` and the row
  said "measuring" forever. Do not lower this back down.
- Measuring from the run's low rather than from the newest sample also catches
  the slow top-up (a phone climbing 1-2 points per 15 min scan), which no
  per-step threshold sees at all. One rule covers both; the per-step check is
  subsumed (low <= newest).
- Tests: `TestBatteryLevelsNoisyLevelKeeps` (80/77/79/74/76 wobble keeps the run
  and still forecasts ~31 days) and `TestBatteryLevelsSlowRechargeResets`
  (60→40 then 42/44/46/48 → series restarted at one sample).

## battery_levels: the forecast inspector (2026-08-10)
- Commits `b51a2b2` (trail + `/api/detail` + tests), `4f86ce5` (page + chromedp),
  `9fc49e0` (DOCS.md). RELEASED in **v4.6.0** (2026-08-10, tag on `363d8d9`).
- Reported symptom, asked for and answered before anything was written: the ETA
  **swings between polls**. Nothing was diagnosed — this is instrumentation to
  find out, not a fix.
- Why a trail and not a log line: the forecast is a pure function of `series[1]`,
  the current level and now, so a swing means the series moved — and both ways
  that happens (a wobbling level appending samples, a `RECHARGE_RISE` wipe)
  destroy the evidence that would explain it. It has to be recording BEFORE
  anyone looks, hence always-on and capped rather than a debug switch.
- `events:<entity_id>`, 40 lines, written only when the series changed, the tier
  changed, or the ETA moved > `ETA_DRIFT` (10%) with no sample behind it. That
  last case is the reported symptom caught in the act. A quiet poll writes
  nothing. Last-reported tier/eta live in an in-memory `reported` table, so the
  suppression check costs no KV read; a reload just re-baselines one line.
- `GET /api/detail?entity_id=` returns the series, the trail and the math,
  read-only ON PURPOSE — inspecting must not sample, or looking at a suspect row
  alters it. `forget_removed` deletes the trail with the series.
- `forecast()` was extracted so `/api/state` and `/api/detail` cannot diverge:
  an inspector that computes the number its own way is worse than none.
- Page: clicking a row expands a panel (a SIBLING of `.row` — the row is a grid
  aligned to the header, nesting a block breaks it). The lead line names the
  guard that failed for a "measuring" row (no samples / no drop / span short of
  MIN_SPAN), which is the single most useful thing on the panel.
- Next time this comes up, the two things to read first: the `wiped`/`low` fields
  on any `reset` event (a run thrown away on noise) and consecutive `sample`
  events whose `drop` alternates (a level wobbling 1-3 points gives the secant
  huge relative variance, which would swing the ETA by the same factor).

## battery_levels: the drain rate is a median (2026-08-10)
- Commits `18b7852` (estimator + tests), `bd33ee5` (DOCS.md). RELEASED in
  **v4.6.1** (2026-08-10, tag on `9a6d28e`).
- The inspector from v4.6.0 did its job on the first dump. Real series off a
  live sensor: `27 28 27 28 27 26 27 26 27 26 27` — a ~0.25 %/day drain under a
  ±1 diurnal swing (high ~10:08, low ~02:08, i.e. temperature). The secant read
  the rate off two single readings, so the answer was decided by which side of
  the swing each endpoint was caught on. Fixture is kept as `wobbleSeries()` in
  the test file; it is REAL user data, do not "tidy" it into round numbers.
- Chosen by measuring variance, not by argument: sliding the poll instant across
  two days of a simulated sensor matched to this one gave secant 2.6x spread and
  silent on 10% of polls, plain Theil-Sen 2.9x, **Theil-Sen with a 12 h minimum
  pair span 1.4x and never silent**, least squares 1.5x but -10.9% biased at the
  4-day window. Scratch programs are gone; rerun by simulating if ever doubted.
- **MIN_PAIR_SPAN is load-bearing, not a refinement.** Without it plain
  Theil-Sen returns exactly 0.0000 on the real series: only three distinct
  levels appear, so 19 of the 55 pairs have Δlevel = 0 and the median lands in
  the pile of zeros. Pair census was 27 negative / 19 zero / 9 positive — the
  trend is in the counts, which a median cannot see. Do not remove it, and do
  not assume "robust estimator" implies robust to *quantization*.
- Fallback when no pair is wide enough (a battery stepping twice within 12 h)
  is to use every pair, otherwise a fast drain would forecast nothing.
- The median only ever describes COMPLETED steps — every pair ends at a sample.
  So `dwell_cap` handles the step in progress: one granularity step (smallest
  observed |Δlevel|) per dwell. It is what replaces v4.4.0's "window ends at
  now" trick. Appending `(now, level)` as a 12th point does NOT work: it
  contributes 11 of 66 pairs and the median stays pinned to the old slope, so a
  60-day stall would still predict last month's rate. Verified, not assumed.
- `TestBatteryLevelsNoisyLevelKeeps` expected 31 days; that was the endpoint
  bias (its endpoints are a high and a low). Median says 1.50 %/day, least
  squares 1.51 independently → 37 days. The old number was wrong, not the new.

## service_api example (2026-08-06)
- New bundled example: `service_api.lua`, one endpoint that calls ANY HA
  service, aimed at shell scripts. Commit 26f0acd (script + tests), ea7f320
  (DOCS.md). RELEASED in v4.1.0 (tag on defe8a2).
- Routes: `POST|GET /call` (prefix, so it also owns `/call/<domain>/<service>`),
  `GET /ping`, `GET /entities`, and `GET "/"` for the builder page. The first
  cut had no `GET "/"` on purpose (a machine-facing API should not become a
  tab); the page reversed that, and `ha.ui` came with it — the debug page flags
  a `/` route WITHOUT `ha.ui`, so the two must travel together.
- Service naming, three ways: path segments, a dotted `service` field, or
  separate `domain` + `service`. Everything not in RESERVED (token/wait/
  domain/service) is forwarded as service data verbatim — that is the whole
  point, no per-service code and nothing to update when HA gains a service.
- Three input transports: query params, form-encoded body (`curl -d k=v`, no
  Content-Type sniffing — a body starting with `{` is JSON, `[` is a 400,
  anything else is form), JSON object body. Body wins over query on collision.
- Type reconstruction for the text transports is the fiddly part:
  `true`/`false` → bool, leading `[`/`{` → json.decode, and a number ONLY when
  `tostring(tonumber(text)) == text`. That round-trip check is load-bearing:
  an alarm `code=0123` must stay a string. `entity_id` is the one field split
  on commas (a comma cannot appear in an entity id, but is ordinary text in a
  notification message).
- Auth: shared token, `crypto.equal` (constant time), accepted as
  `X-Auth-Token`, `Authorization: Bearer`, or `?token=`. Generated with
  `crypto.random_hex(16)` on first load, stored in the script KV and logged at
  warn ONCE; later loads log only a 6-char prefix. Escape hatch for a lost
  token is the `TOKEN` constant, which wins over the store — no magic reset
  key. Rationale: the LAN port (`http_port`) has no HA login in front of it.
- `ha.call_service` is wrapped in `pcall` so an HA rejection becomes a 502 with
  HA's message instead of an on_exception; gopher-lua's `file:line:` prefix is
  stripped from the message first (it means nothing to a curl user).
- GET performing an action is intentional (curl without JSON quoting), noted in
  the script and the commit.
- Builder page (`service_api.html`, `ha.ui("Service API")` + `GET "/"`, commit
  a75d11c): a form that assembles a call and hands back the URL and the curl
  command with copy buttons. New token-guarded `GET /entities` feeds the
  domain/entity pickers from the state mirror; service names CANNOT be
  enumerated (no binding for HA's service registry), so those are a static
  suggestion list over a free-text field — do not mistake it for a whitelist.
- Token delivery, REVERSED on user instruction (commit 356af45): the first cut
  asked the user to paste the token; now `service_api.lua` substitutes it into
  the page (`__SERVICE_API_TOKEN__`) as it serves it. The user was told the
  consequence and reaffirmed: anyone who can open the page on the LAN port has
  the token, so it guards against guessing the URL, not against the network.
  Do NOT "fix" this back to a prompt. The field is still editable (build a
  command for another install) and still verified against `./ping` — its own
  origin, not the typed base URL, which may not resolve from the browser.
- Injection escapes `\`, `"` AND `/` for the JS string literal (a token
  containing `</script>` would end the script block), and substitutes through a
  gsub replacement FUNCTION so a `%` in a hand-picked TOKEN is not read as a
  capture reference.
- Base URL default: page origin + `/s/service_api`, EXCEPT under ingress
  (path contains `/api/hassio_ingress/`), where it falls back to
  `<host>:8100` — the ingress URL carries an HA session curl does not have,
  and the real `http_port` is unknowable from a script.
- The JS `typeOf` mirrors the Lua `coerce` rules (incl. the number round-trip
  check) so the badge next to each value is the truth; if one side changes the
  other must follow.
- Tests: `internal/lua/service_api_test.go` boots the real example with both a
  recording sync and async call_service — every request shape, the coercion
  rules incl. `0123`, token rejection (incl. one-char-short vs the constant
  time compare), the 400/502 error contract, and `wait=false` taking the async
  path. Plus a chromedp test driving the builder: the token verifying live, the
  entity picker filtered to the chosen domain, exact URL/curl output for both
  request styles, and the generated URL fired back at the endpoint to prove it
  actually reaches light.turn_on.

## ikea_dimmer.lua (2026-08-26, `07b56dd`, docs `2514b30`)
- IKEA E1743/RODRET two-button dimmer -> `light.konyha_konyha_led`, via
  Zigbee2MQTT. Requested with real ids; kept in `examples/` like
  mirrored_switches.lua, which also carries the author's real entity ids.
- The device sends `brightness_move_up|down` + `brightness_stop`, never a
  level, so the ramp runs HERE: a chain of `ha.after("250ms")` steps
  (4 s full range). Doing it in the daemon works on any dimmable light
  instead of needing bulb-side level-move.
- There is NO timer-cancel API, so release cannot delete the pending step: a
  generation counter is bumped and every step checks it and disarms itself.
  That also covers a direction reversal with no stop in between. Do not
  "fix" this by adding cancellation to the scheduler for one example.
- No leak from the chain: `timerFns` entries for `after` timers are deleted
  when they fire (`api_ha_test.go` asserts it), and post-load `keepTimer` is
  a no-op, so the pruning list does not grow either.
- Z2M exposes the action twice. `event.<name>_action` (state = timestamp,
  action in the `event_type` attribute) is the reliable one; the legacy
  `sensor.<name>_action` loses repeated identical presses because HA skips
  `state_changed` when state AND attributes are unchanged. The script picks
  the event entity if it exists, else the sensor, else warns and watches
  both.
- Ramp seeding repeats the mirrored_switches echo lesson: the light's
  reported brightness lags the command, so seed from our own last commanded
  level while it is < 5 s old, and only then from the report.
- `ha.immediate_events()` is mandatory here, not an optimization: the 100 ms
  batch window collapses per-entity events, so a short tap's move + stop
  would merge into one and the hold would be lost.
- Everything is traced with `ha.log("debug", ...)` (action received, seed and
  its source, each step, cancellation, release) at the user's request — one
  press becomes a burst of async service calls, and there is nothing else to
  reconstruct it from.
- Tests: `internal/lua/dimmer_test.go` runs the real script against a spy
  call_service with a LIVE scheduler (the ramp is real timers): clicks, a
  two-step ramp then a release that must silence the next step, the dim-down
  clamp at MIN, hold-up-from-off lighting at MIN first, and unknown/empty
  actions being ignored.

## ikea_dimmer.lua input path (2026-08-26, `88c88eb`)
- Field report: the script did nothing. Cause: on the author's Zigbee2MQTT 2.x
  the dimmer is published as an **MQTT device trigger**
  (`device_automation` discovery, `discovery_id: 0x… action_on`). That is NOT
  an entity and NOT a bus event — HA's mqtt device_trigger subscribes to the
  topic and calls the automation directly, so nothing on the WebSocket API
  can observe it. No amount of entity-name guessing would ever have worked.
- Fix: the press is bridged by ONE HA automation firing `ha_lua_command`
  (`script: ikea_dimmer`, `action: "{{ trigger.payload }}"`), consumed with
  `ha.on_command`. Deliberately did NOT invent a new event type: on_command
  already does the addressed-to-this-script filtering. `trigger.payload` is
  the action string — confirmed from the author's existing automation.
- The action-entity path is kept for installs that have one, resolved by
  scanning `*_action` (event domain before sensor) instead of assuming the
  id; the chosen input is logged at info, since "does nothing" is this
  script's failure mode.
- If ha-lua ever wants these presses without an HA automation in the middle,
  the only real answer is an MQTT client in the daemon. Not worth it for one
  bridging automation.
- Found while wiring it: `ha.on_event` handlers are called with the event's
  DATA table, not the {event_type, time_fired, data} envelope lua_api.md
  showed (runner.go:446). Docs fixed in `89fcc2a`.

## ikea_dimmer.lua ramp shape (2026-08-26, v4.7.1)
- Field report: "I can see the steps". Cause was a LINEAR step (+16 of 255):
  perceptually enormous at the dim end, invisible at the bright end. Now
  geometric — level *= (MAX/MIN)^(step/full) — so every step looks the same
  size. Near the floor the multiplicative step rounds back onto itself, hence
  the ±1 floor in scale(); without it the ramp stalls short of MIN.
- Rate: RAMP_FULL_SECS 8 at a 150 ms cadence (user asked for half of 4 s).
  Raising RAMP_FULL_SECS slows the ramp AND shrinks each step, so it is the
  only knob worth exposing.
- `light.konyha_konyha_led` reports supported_features=40 (TRANSITION|FLASH),
  color_modes=brightness. Transition IS honoured, so each step glides over
  the step interval and the chain reads as continuous. Do NOT replace it with
  one long transition: the device fades linearly in brightness units, which
  perceptually does nothing and then falls off a cliff.
- The dangling `STEP` in the ramp-start trace printed "&{}" instead of
  raising — gopher-lua's %d on nil. A trace-only reference to a deleted
  constant survives every test; grep for old constant names after a rename.
