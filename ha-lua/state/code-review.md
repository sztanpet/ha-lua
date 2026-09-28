# State: code review rounds

What each review round checked, so the next one does not redo it. The fixes
themselves are in git and CHANGELOG.md. Global decisions live in
`../AI.state`.

| round | date | scope | outcome |
|---|---|---|---|
| 1 | 2026-07-01 | all non-test Go, the card | 8 fixes, v2.8.9 |
| 2 | 2026-07-25 | all non-test Go | 5 fixes, v3.2.2 |
| 3 | 2026-09-24 | all non-test Go | 2 defects + refactors, v4.9.1 |
| 4 | 2026-09-25 | Go, Lua examples, card, web assets | 3 fixes, v4.10.0 |
| 5 | 2026-09-28 | enhanced climate + overshoot | 9 fixes, v4.15.0 |

Method that worked every round: confirm a suspected bug with a throwaway probe
test before writing it up, and verify the regression test fails on the old
code. Where a race window is a few instructions wide, a no-op prod hook var
(`stopScriptHook`, like `smtpSendMail`) parks the test in the gap.

## Round 5 — still open
- **Deploy v4.15.0** (the user's go-ahead): new add-on image + restart, re-copy
  `enhanced_climate.lua`, `.html`, `lib/control.lua`, `lib/overshoot.lua` into
  `/config/ha-lua/scripts/`, restart again. Then set the children's room once
  from the card: its last hold lapsed 2026-09-28 18:11, and nothing controls it
  until the next dial change.
- **D1** (on the box, the user's go-ahead): in the Z2M frontend, temp8 →
  Reporting → msTemperatureMeasurement: min 10 s, max 300 s, change 10
  (0.1 °C). It costs battery. A day later check the recorder for 0.1 steps
  within minutes. If the firmware ignores it: the pvxx ZigbeeTLc firmware
  (Z2M model `ZG-227Z-z`) or another sensor.
- Deferred until seen: put the request back when control lapses (schedule
  cleared or climate removed) with our frost or hold on the device; a stale
  echo still carrying a setpoint we just replaced reading as a dial change.

## Raised and REJECTED by the user — do not re-raise
- `scripts/lib/` is not watched (2026-09-25). Do not add a recursive watch or
  "fix" the DOCS wording into a caveat.
- `/debug/` on the unauthenticated LAN port (2026-09-25): LAN-trust by design.

## Checked and NOT changed — don't re-derive
Daemon
- `ha/client.go` reconnect/backoff, pending-command drain, subscribe
  bookkeeping (`conn` and `subscribed` swap in one critical section).
- Registry/Supervisor stop ordering; lock order `Supervisor.mu →
  {Registry.mu, Scheduler.mu}` and `Scheduler.mu → Registry.mu`, no cycle.
  `Runner.Close()` vs `Dispatch`: no send-on-closed path.
- `scheduler.fireDue` holding the heap lock across DB writes and `onFire`.
- `msgID` is `atomic.Int32` on purpose (32-bit `int` on armv7 HA boxes).
- `purge.go`'s `…Z` cutoff vs HA's `…+00:00` `changed_at`: the lexical compare
  can only differ within the same second.
- Event coalescing keeps the newest state at the entity's FIRST arrival
  position, so it can reorder against a later custom event. Documented;
  `ha.immediate_events()` is the escape hatch.
- In-place slice filtering in `Router.Unregister`/`Scheduler.RemoveScript`;
  `cachedEventHandlers` read vs append-at-len; `stdlib.go` deleting from the
  `os` table while ranging it.
- Seed has FULL-SNAPSHOT semantics: tests upsert one entity via
  `HandleStateChanged`, never repeated one-entity Seeds.
- `haAPI.keepIDs` (all load-time timer ids, `after` included) is what
  PruneScript keeps; `timerSeq` numbers every/at only. Do not merge them.
- Slow-loris on the LAN port, negative `GetHistory` limit, `logbuf` re-parsing
  levels per snapshot, `purge.exec`'s `(0, nil)` on a RowsAffected failure,
  `err != http.ErrServerClosed`: all fine.
- `cmd/ha-lua` at 0 % coverage is accepted.

Refactors decided
- Keep duplicated: `web.Start` vs `debug.Start`, `cards` vs `examples`
  Materialize. Merging costs more than the copies.
- Round 3 deliberately REVERSED two round-1 rejections: store
  `Store`/`GlobalStore` share one `table` (SQL still written out verbatim), and
  `registerHaAPI` is split by area (it had grown to 460 lines). Do not flip
  back.

Card and examples
- Card rewrite (e.g. Lit) rejected: it would reopen every lifecycle bug for no
  visible gain.
- The card's configure Set keyed `entity|hash`, the preview guard, the
  `_relevantChanged` render gate, the stepper echo + debounce, and the schedule
  round trip.
- Every `innerHTML` in pages and web assets is static markup.
- Overshoot: observe-only converges on real runs (c 0.0205 from 8 runs);
  MIN_LEAD discards bias c down and bound the armed learner; load-time
  abandonment, first-reason-wins, the journal ring, invalidation on window,
  mode and setpoint changes.
- The radiator sensor (60 s, 1/64 °C) is fine. IKEA window contacts bounce
  off/on within 1 s, costing one extra write pair — the correct reaction.
