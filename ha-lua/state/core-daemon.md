# State: core daemon (plan.md)

Spec: `plan.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE** — milestones 1–12, add-on 1.0.0, stable since. Later
tracks build on it; code review rounds are in `code-review.md`.

## Gotchas not visible in the code
- **honnef.co/go/tools sits on v0.8.0-rc.1.** v0.7.0's IR builder panics on the
  Go 1.27 stdlib (`unexpected expr: *ast.KeyValueExpr`), taking down both
  `make staticcheck` and `make lint` (golangci-lint vendors the same pass).
  Drop the `-rc` once v0.8.0 is out.
- `go-json-experiment/json` still shows up as an indirect dep: chromedp pulls it
  in. Ours is the stdlib `encoding/json/v2`.
- **Dockerfile: no GOARCH mapping.** home-assistant/builder builds emulated
  under the target arch, so plain `go build` is right; mapping aarch64→arm64 by
  hand is the classic broken-image trap.
- The Go const `ingressPort = 8099` must equal `config.yaml`'s `ingress_port`;
  it is forced in add-on mode only, dev mode binds just the LAN port.
- Release CI is tag-only, by user decision: no PR test/lint workflow.
- golangci-lint's `install.sh` has a checksum bug (greps the `.sbom.json`
  line). Verify the release tarball against its sha256 by hand when bumping.
- The daemon blocks on the first HA state seed, so it serves nothing — UI
  included — without a reachable HA.
- Purge runs once at Start before its first tick; on the 1 h default a
  frequently restarted daemon would otherwise never purge.
- Hot-reload stop: Registry.Remove → Runner.Close (drain) → wait 5 s, then
  cancel the script ctx (aborts the VM). OnLoaded adds each script's event
  types to the HA subscription and never unsubscribes.
- The per-script event buffer is 256; batching, not the buffer, is what stopped
  the drops.

## Benchmarks
- `make bench-update` re-RUNS the suite; to promote an existing run,
  `cp benchmarks/current.txt benchmarks/baseline.txt`.
- One `make bench-compare` against an old baseline is noise-prone (a +4.7 %
  "regression" from pprof labels was machine noise). Re-measure both sides in
  one session before believing a delta.
- pprof goroutine labels: every goroutine runs under `pprof.Do`; the one that
  matters is `goroutine=script, script=<id>`, since every script runs the same
  interpreter loop and a stack alone cannot say which one burns CPU.

## Database compaction (2026-10-16)
- Nothing compacted the DB before; 166 MB on the box at 2-day retention. The
  purge now VACUUMs + `wal_checkpoint(TRUNCATE)` when freelist_count /
  page_count > 0.25 (`8ca609e`); the debug page shows the free bytes
  (`ced947d`). Released as v5.2.0.
- Why a threshold and not `auto_vacuum`: a default purge frees ~1/48 of the
  rows and inserts reuse those pages, so steady state needs nothing; only a
  lasting shrink (lower retention, an aged-out flood) leaves space for good.
  auto_vacuum=INCREMENTAL would itself need a full VACUUM to switch on, and
  VACUUM holds the single write handle for its duration.
- Deployed 10-16 22:32 CEST. First purge vacuumed at once: freelist 83 MB of
  166 MB (50%, not the ~2% expected; likely the dropped `states` table and
  older floods), file 166 -> 51 MB, 1.2 s. The result is smaller than
  size minus freelist: VACUUM also repacks part-full pages, so the freelist
  understates what a vacuum reclaims. Free after: 90 KB.
- INCREMENTAL (user, 10-16: "implement incremental vacuuming"): OpenDB
  switches the file to auto_vacuum=INCREMENTAL (one VACUUM + WAL truncate for
  an older file, `f441d10`); every purge then runs `PRAGMA incremental_vacuum`
  + `wal_checkpoint(TRUNCATE)` when the freelist is non-empty (`a43f324`),
  replacing the 25% threshold. Released as v5.3.0. Note: incremental vacuum
  does not repack part-full pages the way VACUUM does.
  Deployed 10-16 22:49 CEST: conversion took 539 ms on the 52 MB file; the
  first purge deleted 1653 rows and released 606 KB in ~40 ms; free 0 after.
- Dev config renamed (10-19, v5.5.1, `92d3a7d`): the Supervisor globs
  `config.*` with a yaml/yml/json suffix anywhere in an add-on repository
  and parsed `config.dev.yaml` as a manifest ("Invalid app config!" on
  every store reload). Never name a repo file `config.<ext>` again. The
  same log's StoreGitError for repo `8e82c099` is ljsquare/hassio-addons
  (GitHub 404, nothing installed from it) — the user's to remove.
  Deployed 10-19 10:53 CEST; store reload logs no manifest warning.
