# State: bundled reference examples (load-examples-spec.md)

Working state for the read-only bundled examples tree. Spec:
`load-examples-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE.** Shipped in 2.2.0 (2026-06-22, tag v2.2.0).

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
  page, tests), be24e8c (DOCS.md section). NOT released yet.
- The design decision worth remembering: the ETA cannot come from
  `ha.get_history`. State history is purged after `retention_days` (2 by
  default) and a battery forecast needs weeks. So the script keeps its OWN
  series in its KV store (`series:<entity_id>`), one sample per observed level
  change — a handful of rows a month per entity, capped at MAX_SAMPLES=120.
- Fit is least squares over the samples PLUS the current instant as a point.
  Appending "now" is what makes a battery that stopped moving decay towards a
  flat slope instead of forever predicting last month's rate.
- A rise > RECHARGE_JUMP (2 points) wipes the series: a charge or a swap makes
  every earlier sample worthless. Guards before any ETA is shown at all:
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
