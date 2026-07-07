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
