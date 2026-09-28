# State: event-to-action latency (event-latency-spec.md)

Spec: `event-latency-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE** — M0–M5 in v3.1.0 (2026-07-07); the `states` table was
retired the same day.

## Where it landed (dev machine, `internal/e2e`)
- event → command: mean ~150 µs, p99 ~0.35 ms; a busy KV is identical to idle.
- Quick on→off: ~0.2–0.4 ms with `wait = false`, ~100 ms synchronous (it waits
  for HA's result frame, which HA sends only after the Zigbee round trip).
- The remaining gap to HA's built-in automations is the WS hop (~1–2 ms):
  inherent to being out of process.

## Decisions and why
- The memory mirror had to come before async persistence: async-only writes
  would let `ha.get_state` race the queue.
- Seed dedup baseline: memory when populated (reconnect), the newest history
  row per entity on cold start (the SQLite mirror is gone).
- Rejected: `sync.Map` (one writer dominates, RWMutex is clearer), per-event
  writer goroutines (lose ordering and batching), memory-only seed dedup
  (a phantom history row per entity per restart).
- **Echo guards in mirror scripts: attribute, don't compare.** Comparing
  against a partner's REPORTED state lags its COMMANDED state by the Zigbee
  round trip, so fast toggles lost a command and the late echo bounced the
  switch back. The pattern is a per-entity FIFO of expected reports
  (`mirrored_switches.lua`, `group_switches.lua`).

## Worth knowing
- The runner logs queue-to-handler delay per event at debug, warn ≥250 ms
  (clear of the batch window, so a warn is real). At `log_level: debug` that
  line floods the log: 12k lines in 25 min, so the 5 MiB budget rotates in
  about an hour. At debug the log is a rolling window, not a record.
- Untouched spike sources, if variance ever returns: WAL autocheckpoint and the
  hourly purge DELETE, both on the write connection. Never measured as a
  problem.
- e2e harness: `startPipeline` asserts the script does
  `global.set("loaded", "bench")`, and the fake HA's `injectStateChanged` sends
  no `old_state`. A shipped example needs both addressed before it can be
  benchmarked there.
