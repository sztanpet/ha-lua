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
