# State: heating overshoot correction (overshoot-spec.md)

Working state for the learned early-cutoff correction. Spec:
`overshoot-spec.md`. Global decisions live in `../AI.state`.

Status: **spec written, nothing implemented.** Next: commit 1 of
`overshoot-spec.md` §11 (`thermostat: publish the written setpoint
separately`).

## Why it exists (2026-09-06)
- Field problem: the children's room is small and its thermostat sails 1–2 °C
  past setpoint on every scheduled warmup. The room is an ESPHome node —
  `climate: platform: thermostat`, relay into a thermal actuator, regulating
  against a separate room sensor.
- The overshoot is stored energy released *after* the cutoff decision: the wax
  head takes 2–4 min to close, and the radiator body then radiates for another
  10–20 min. A small room has too little mass to absorb it. Bang-bang has no
  knob for this — `heat_overrun` only coasts further past, `heat_deadband` sets
  the switch-on point, and neither goes negative.

## Design path (how we got to §5)
Worth keeping because three plausible designs were tried and discarded in
order, each for a reason that is not obvious from the final shape:

1. **Predictive cutoff from a 10-minute rate of rise** — first proposal.
   Rejected by the user: a derivative over a 0.1 °-resolution sensor reporting
   on change is mostly quantization noise.
2. **PID on the ESP (`platform: pid` + `slow_pwm`)** — the textbook answer, and
   the assistant's recommendation until the user pushed back. Both objections
   were correct and are now spec §4.1: it costs warmup speed *by construction*
   (output saturates only while error is large, so the last degree runs at
   ~40 % duty and falling), and fixed constants drift with the seasonal plant
   gain. The deeper point: PID is for unpredictable disturbances, and this one
   repeats every night — discarding that repeatability is what made it the
   wrong tool.
3. **A flat learned offset** — the shape proposed after dropping PID, and wrong
   in steady state: it depresses the *hold* band by the full offset, not just
   the warmup peak, leaving the room permanently cold. Surfaced while writing
   the spec, before any code. Fixed by making the offset proportional to the
   rise being attempted (§5), which collapses to ~zero for a top-up and lets a
   single learned scalar cover both regimes.

## Decisions not to re-litigate
- **The ESP keeps `platform: thermostat`.** Full relay power for the whole
  approach is what preserves warmup speed; only the number it stops at moves.
- **The offset is latched at episode start, never recomputed per tick.**
  Recomputed, `command` climbs as `rise` shrinks and converges on `requested`
  without ever cutting early — the correction silently does nothing. Easiest
  thing to get wrong in the whole design.
- **`K_INIT = 0`.** Having learned nothing, it must behave exactly as today —
  never worse than the status quo on day one.
- **The coast peak is sampled on the 1-minute tick, not read from
  `ha.get_history`.** The user's instinct to compare against yesterday was
  right in spirit, but the learner only needs one number per episode, not a
  replayed curve. Avoids `get_history`'s missing `until` parameter (~1500 rows
  pulled and filtered in Lua) and needs no retention override.
- **A standalone script cannot do this.** It would be clobbered by the 60 s
  tick and, worse, latched as a manual hold by `thermostat.lua:196`. The
  `desired`/`written` split (§7) is the minimum controller change.
- **No `valve_watch.lua` change needed** — and specifically *because* PID was
  dropped. Under `slow_pwm`, `hvac_action` churns every period and
  `demand_continuous` (which requires every row in its window to show demand)
  would have gone permanently false, silently disabling the seized-valve alarm
  on that zone. Bang-bang keeps it clean. Noted here because an earlier commit
  plan carried a valve_watch fix that is no longer warranted.

## UI rule
Requested temp is the primary number; commanded, offset and sample count are
reachable only by a deliberate **tap** — not a `title=` tooltip, which is
hover-only and therefore unreachable on the phones and wall tablets this UI
actually runs on. Payload splits `target` (requested) from `commanded`; the
stepper must edit `target`. Left unsplit, `target` silently becomes the
commanded value and one stepper nudge ratchets the user's real request down by
the offset.

## Introspection rules (spec §9)
Added on the user's instruction, 2026-09-06, and they are design rather than
polish — this is the only script here that fails *silently*. Everything else
raises or notifies; a learner just sits there with a wrong number in it.

- **A discarded episode is a record with a reason, never a bare `return`.** A
  learner that silently discards every episode looks exactly like one that has
  converged: `k` still, no error, no log line, room still overshooting. If a
  window is opened during the warmup every evening, the feature would do
  nothing at all and say nothing about it.
- **Discards log at `warn`, not `debug`.** If you must raise the log level to
  find out the feature has never once run, the diagnostic has already failed.
- **The validity predicate returns `ok, reason`, not `ok`.** One string shared
  by the journal, the log line and the unit tests. A boolean makes the caller
  re-derive the reason for the journal and the two copies drift.
- **Observe-only ships defaulted ON.** Computes, journals and logs the offset;
  writes the uncorrected value. One branch at the write site, and it buys a
  week of evidence about what it *would* have done before it goes near a
  child's bedroom. Turning it off per zone is the deliberate act of trusting
  it.
- **`k` resets from the UI/HTTP, never from `sqlite3 /data/ha-lua.db`**, and
  without a restart or reload.
- **No daemon changes.** The debug page's accessors never touch an `*lua.LState`
  (standing project decision), so learner state cannot go there and should not
  — the script serves its own page and its own source-filterable log lines.

## Pending
- All five commits of §11.
- `enhanced_climate.lua` + the Lovelace card are deferred (§12); the
  children's room is a `lib/zones.lua` zone, so `thermostat.lua` is the target.
