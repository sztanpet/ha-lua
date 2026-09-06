# Heating overshoot correction — Specification

> **Working state:** [`state/overshoot.md`](state/overshoot.md) — implementation progress and decisions.

Status: **ready to build**. Control model and UI rules resolved (§5, §8);
§12 lists what is deliberately deferred.

## 1. Goal

Stop small rooms from sailing past their setpoint on a scheduled warmup,
**without slowing the warmup down**.

The motivating case is the children's room: an ESPHome node with a separate
room sensor, `climate: platform: thermostat` (bang-bang), driving a relay into
a thermal actuator on the radiator. It reaches its setpoint and then keeps
climbing 1–2 °C.

## 2. Non-goals

- **Not a replacement for the ESP's controller.** The node keeps
  `platform: thermostat` and keeps regulating against its own room sensor. All
  ha-lua does is choose the number it regulates *to*.
- **Not a swing/amplitude fix.** This targets the peak on a warmup from
  setback, not the size of the steady-state cycle. Cycle amplitude is set by
  `heat_deadband` on the node and is a separate, cheaper lever.
- **No new hardware, no new HA integration.** The interface stays
  `climate.set_temperature` on a climate entity.
- **No outdoor-temperature model, no weather compensation.** The learner
  tracks seasonal drift by continuously re-measuring (§6), not by modelling it.

## 3. Why the room overshoots

Three lags stack, and all of them release *after* the controller has already
decided to stop:

1. `platform: thermostat` is a switch, not a proportional band. It drives the
   relay **full on** until the room reaches setpoint.
2. The thermal actuator takes 2–4 minutes to *close* once the relay drops. Hot
   water keeps flowing for all of it.
3. The radiator body is then still at 50–70 °C and keeps radiating for another
   10–20 minutes.

A small room has little air and little mass to absorb that stored energy, so it
lands as overshoot. **No knob on the `thermostat` platform can fix this**:
`heat_deadband` sets the switch-*on* point below setpoint, `heat_overrun` only
lets it coast *further* past, and neither can be negative. A switch cannot
anticipate.

The energy is real and already in the metal. The only lever is to **stop
earlier** and let it land on target instead of above it.

## 4. Rejected alternatives

### 4.1 PID on the ESP (`climate: platform: pid` + `slow_pwm`)

The textbook answer, and wrong here for two reasons:

- **It costs warmup speed, by construction.** A PID's output saturates only
  while the error is large. With a `kp` conservative enough not to oscillate on
  a plant this laggy, full duty holds down to roughly 2.5 °C of error and then
  throttles — the last degree of an 18→21 warmup runs at ~40 % duty and
  falling, with `kd` pulling it lower still on the way up. That is not a tuning
  failure; it is the rise-time/overshoot tradeoff PID *is*. Tune it not to
  overshoot and the last mile is lazy.
- **Fixed constants drift.** Tuned at 5 °C outdoor, the same constants meet a
  different plant gain at 15 °C outdoor — same radiator output, less loss,
  higher gain, overshoot returns. Nothing in a fixed-gain PID notices. Seasonal
  retuning, plus an autotune run that takes most of a day on a plant this slow.

The deeper objection: PID is the right answer when the disturbance is
unpredictable. **This one isn't.** The residual heat dumped after cutoff is
close to identical every night — same room, same radiator, same schedule. PID
discards that repeatability and re-derives the answer every cycle. Exploiting
it instead is both simpler and faster.

### 4.2 A flat learned offset (command `requested − offset`)

Fails in steady state. The offset lowers the commanded setpoint *permanently*,
so once the room is warm it holds around `requested − offset` rather than
`requested` — the room ends up simply too cold, all day, by exactly the amount
we cut. The big overshoot only ever happens on the **first long run from
setback**; short top-ups barely warm the radiator and barely overshoot. A
correction that does not distinguish the two regimes is wrong for one of them.
§5 makes the offset proportional to the rise being attempted, which collapses
to ~zero for a top-up.

### 4.3 A standalone script that never touches the controller

Attractive — it is how `heating_windows.lua` cooperates — but it cannot work
for a *setpoint* correction. Both controllers tick every 60 s:

- `thermostat.lua:175` — `control.should_write` sees the corrected target ≠
  published desired and writes the uncorrected value straight back.
- `thermostat.lua:196` — worse, the manual-change detector reads the correction
  as the user turning the dial and latches a **manual hold at the corrected
  value until the next schedule transition**, freezing the room.

Writing the *hvac mode* instead (`off`/`heat`) would be yielded to by both
controllers — `control.should_write` returns false when `mode ~= "heat"`, and
both manual detectors bail on `state ~= "heat"` — but mode-flapping every cycle
is ugly in history and drops frost protection. §7 applies the correction inside
the controller instead, which is a small contained change.

## 5. The control model

**Full power to a computed early cutoff.** The ESP is left in bang-bang, so the
relay is hard on for the entire approach — warmup speed is unchanged from
today. Only the number it stops at moves.

An **episode** opens when the requested setpoint rises above the room
temperature (a schedule transition, an override, a manual hold). At that
moment, and only then, the controller latches:

```
rise   = requested − current                    -- how far it has to climb
offset = clamp(k * rise, 0, MAX_OFFSET)         -- k is the learned scalar
command = requested − offset
```

`command` is held for the whole episode. The episode closes when the room first
reaches `requested`, or when the requested setpoint changes again.

**The offset must be latched at episode start, not recomputed per tick.** If it
were recomputed, `command` would climb as the room warmed (`rise` shrinking
toward zero) and converge on `requested` without ever cutting early — the
correction would silently do nothing. This is the single easiest thing to get
wrong here.

Proportional-to-rise is what makes one scalar cover both regimes: an 18→21
warmup with `k = 0.4` cuts at 19.8, while a 20.7→21 top-up cuts at 20.88 —
inside the deadband, i.e. no correction at all. The steady-state hold band is
left where the user asked for it (§4.2).

This is **not** a proportional band. A P controller throttles output power as
it approaches; this holds full power and moves the stopping point. Fast *and*
early.

### 5.1 Constants

| Name | Default | Meaning |
|------|---------|---------|
| `K_INIT` | `0` | Starting coefficient. Zero means the first episode behaves exactly as today — never worse than the status quo while it has learned nothing. |
| `GAIN` | `0.5` | Fraction of the measured error folded in per episode. Converges in ~4–5 episodes, damped enough not to ring. |
| `K_MAX` | `0.8` | Hard bound on `k`. Sanity only; a plant needing more than this is broken elsewhere. |
| `MAX_OFFSET` | `2.5 °C` | Absolute cap on a single cutoff, whatever `k * rise` says. |
| `MIN_RISE` | `0.3 °C` | Episodes smaller than this teach nothing (the overshoot is noise) — the correction still applies, but no learning happens. |
| `COAST_WINDOW` | `30 min` | How long after cutoff the peak is watched for. |

## 6. The learner

One scalar per zone, `k`, in the script's KV store. After each episode:

```
peak  = max room temperature observed within COAST_WINDOW of the cutoff
error = peak − requested                        -- >0 too hot, <0 undershot
k     = clamp(k + GAIN * error / max(rise, MIN_RISE), 0, K_MAX)
```

A discrete integral controller closed across days. Because it corrects on
**measured outcome**, it tracks seasonal drift on its own: when a milder month
raises the plant gain and overshoot creeps back, the next few episodes push `k`
up without anyone retuning anything. That is the direct answer to §4.1's
brittleness.

**The peak is sampled from live state on the existing 1-minute tick, not from
`ha.get_history`.** The coast peak is a broad 20-minute hump, so 1 Hz/min
sampling is ample. This deliberately avoids the history path: `ha.get_history`
has no `until` parameter, so "yesterday 06:00–08:00" would mean pulling ~1500
rows and filtering in Lua, and would need a retention override to survive the
2-day default. Tracking a running max costs one store write per tick instead.

**Episodes that are discarded** (correction applied, nothing learned):

- a window opened at any point during the episode or its coast — that is
  `heating_windows.lua`'s territory and the thermal picture is meaningless
- the hvac mode left `heat`
- the requested setpoint changed before the room reached it
- `rise < MIN_RISE`
- the daemon restarted mid-episode — in-flight episode state is abandoned, not
  reconstructed. One lost sample is worth nothing; a corrupted `k` is.

## 7. Controller integration

The correction is applied inside `thermostat.lua`, at the write. That breaks an
assumption the manual-change detector rests on — that the controller always
writes exactly what it published (`lib/control.lua:31`) — so the two values are
split:

| global key | value | consumed by |
|------------|-------|-------------|
| `thermostat:desired:<zone>` | **requested** — what the user asked for | UI, card, anything showing intent |
| `thermostat:written:<zone>` | **commanded** — what was actually sent | `control.is_manual`, `heating_windows.lua`'s restore-on-close |

`desired` keeps its current meaning and its current name, so nothing that reads
it today changes. `is_manual` and the window restore move onto `written`,
which is the value actually on the device — restoring `desired` after a window
closed would otherwise wipe the correction for the rest of the episode.

`enhanced_climate.lua` has the same two sites (`:314`, `:365`) and takes the
same treatment if the feature is extended to it (§12).

## 8. UI

**The requested temperature is the primary number. The commanded value is
reachable only through a deliberate action.** Showing 19.8 where the user set
21 reads as a bug or a failed write. But it must stay reachable: when the
learner misbehaves — `k` pinned at `K_MAX`, room never warming — that number is
the only way to see why.

**The disclosure is a tap target, not a `title=` tooltip.** The page runs under
HA Ingress on phones and wall tablets, where hover does not exist and a `title`
attribute is simply unreachable — a hover tooltip would be missing exactly on
the device you would be holding while wondering what the thermostat is doing.
Tapping the setpoint reveals a subline; a dotted underline marks it without
advertising it.

The zone state payload (`thermostat.lua:270`) splits accordingly:

| field | source | role |
|-------|--------|------|
| `target` | `desired` | the requested setpoint |
| `commanded` | `written` | revealed on tap |
| `offset`, `k`, `samples` | the learner | revealed on tap, beside `commanded` |

`target` currently reads `current_target(zone)` — straight off the climate
entity — so left alone it silently becomes the *commanded* value for every
consumer of the API, under a name that says "what was asked for". Fix the
meaning before the two values can ever differ.

**The shipped card does not read `target`, and there is no ratchet risk.** An
earlier draft of this section claimed the stepper edits from `target` and that
one nudge would therefore walk the user's request down by the offset. That is
wrong: `thermostat.html`'s stepper edits `override_temp`, a KV value the
correction never touches, and the card renders `current_temp`, `override_temp`
and the schedule strip only. `target` is unread by our own UI today.

**Which means the card has no setpoint display at all.** The head shows the
room temperature and a status word; the only number on the card is the override
temperature. So §8 is not a relabelling of a number already on screen — commit 5
has to *add* the requested setpoint to the card and hang the disclosure off it.
That is the honest reading of "show the requested temp", and it is more work
than revealing a second value beside an existing one.

Show `offset` and `samples` together, not a bare commanded number: "19.8°,
−1.2° learned over 6 nights" says whether to trust it; "19.8°" says nothing.

**Unclosable leak:** HA's native thermostat card, and the node's own display,
read the climate entity directly and will show the commanded value with no
explanation. The requested/commanded split only holds inside our own UIs.

## 9. Debugging and introspection

Every other script here fails loudly and immediately: a bad service call raises,
a seized valve notifies. **This one does not.** It accumulates hidden state over
days, from episodes nobody was watching, and its failure mode is a room that is
quietly 1.5 °C too cold in February because of an episode last Tuesday that
learned from bad data. Delayed, unreproducible, silent. Introspection is
therefore not a convenience here — without it the feature is not debuggable at
all, and the rules below are part of the design rather than a later addition.

### 9.1 Every episode is journaled, discarded ones included

**A discard is a record with a reason, never a bare `return`.** This is the
central rule. A learner that silently discards every episode is
indistinguishable from one that has converged: `k` sits still, nothing errors,
no log line appears, and the room keeps overshooting. If a window is opened
every evening during the warmup, or the mode keeps leaving `heat`, the feature
would do nothing whatsoever and give no sign of it.

One record per episode, appended to a bounded ring (last 50 per zone) in the
script's KV store:

```
opened_at, closed_at, zone
requested, current_at_open, rise
k_used, offset, commanded          -- what it decided, and from what
peak, peak_at, error               -- what actually happened
k_before, k_after                  -- what it concluded
outcome  "learned" | "discarded" | "observed"
reason   nil | "window_open" | "mode_left_heat" | "setpoint_changed"
              | "rise_too_small" | "restart"
```

Deciding inputs and resulting action are both in the record, so an episode can
be re-judged months later without the surrounding state.

### 9.2 The validity check returns a reason, not a boolean

Design constraint on `lib/overshoot.lua`: the episode-validity predicate returns
`ok, reason`, not `ok`. The journal, the log line and the unit tests then assert
on the same string. A boolean forces the caller to re-derive the reason for the
journal, and the two copies drift — the classic way a diagnostic ends up lying.

### 9.3 Log lines at all three decision points

Via `ha.log`, so they land in the daemon log and are filterable by source in the
existing debug page's log viewer (`internal/web/debug.go`) with no daemon change:

| point | level | carries |
|-------|-------|---------|
| episode open | `info` | zone, requested, current, rise, k, offset, commanded |
| episode close | `info` | peak, error, k before → after |
| **discard** | **`warn`** | the reason from §9.2 |

Discards are `warn`, deliberately, and not `debug`. A persistent discard is
exactly the silent failure of §9.1, and it must be visible at the default log
level — if you have to raise the log level to discover the feature has never
once run, the diagnostic has already failed.

### 9.4 Observe-only mode, and it ships enabled

A per-zone flag. The controller computes the offset, journals it and logs it,
but writes the **uncorrected** setpoint. Everything runs and records; nothing
touches the heating.

**This is how the feature ships first, defaulted on.** It costs one branch at
the write site, and it means the learner can be judged on a week of what it
*would* have done before it is allowed near a child's bedroom. Turning it off
per zone is the deliberate act of trusting it — which is also the only honest
way to answer "is `k` converged yet", since the journal shows the predicted
peak against the real one either way.

### 9.5 `k` is resettable without touching the database

A UI action and an HTTP endpoint that zero `k` and clear the journal for one
zone. When a learner goes wrong the recovery path must not be
`sqlite3 /data/ha-lua.db`, and it must not require a daemon restart or a script
reload.

### 9.6 Surfacing

- **On the page:** `k`, the current offset and the last episodes, behind §8's
  tap disclosure — the same action, one level deeper. Requested stays the only
  number on the default view.
- **As JSON:** `GET /zones/<zone>/overshoot` returns `k`, sample count and the
  journal, so it is curl-able and greppable without the UI. `thermostat.lua`
  already has the HTTP API section for it.

### 9.7 What is deliberately not added

**No daemon changes.** The debug page's accessors never touch an `*lua.LState`
(a standing project decision), so per-script learner state cannot be surfaced
there — and should not be. It is script state; the script already serves its own
page and its own filterable log lines. Nothing here needs Go.

## 10. Interaction with `valve_watch.lua`

Benign, but worth stating because an earlier draft of this design (PID +
`slow_pwm`, §4.1) would have **silently disabled** valve_watch on any corrected
zone: PWM toggles `hvac_action` heating→idle every period, and
`demand_continuous` (`valve_watch.lua:96`) requires *every* history row in its
15-minute window to show demand. Staying with bang-bang keeps `hvac_action`
clean and that failure never arises.

What does change: episodes now end below the requested target, and runs get
shorter by the cutoff. `demand_active`'s fallback compares against the climate
entity's `temperature` attribute — the commanded value, which is what the node
actually regulates to — so it stays consistent. Warmups from setback remain far
longer than `WARMUP`, so judging opportunities are unaffected in the case that
matters. **No change to `valve_watch.lua` is required.**

## 11. Files and commit order

Each commit compiles and passes `make test`.

1. **`thermostat: publish the written setpoint separately`** — add
   `thermostat:written:<zone>`; move `control.is_manual` and
   `heating_windows.lua`'s restore onto it. Pure plumbing; behaviour identical
   while `written == desired`.
2. **`thermostat: split requested and commanded in the zone payload`** —
   §8's payload fields: `target` re-based onto the request, `commanded` added.
   Still a no-op for the shipped UI, which reads neither, but it must precede
   any non-zero offset so no consumer ever sees a corrected `target`.
3. **`examples: learn each zone's heating overshoot`** — `lib/overshoot.lua`
   (pure: the latch, the clamp, the `k` update, and the `ok, reason` validity
   predicate of §9.2) with Go unit tests alongside `lib/control.lua`'s;
   `overshoot.lua` does the I/O, episode detection and peak tracking. Ships
   with §9.1's journal and §9.3's log lines — the diagnostics land *with* the
   learner, not after it, because the first week of episodes is the data that
   says whether any of this works.
4. **`thermostat: apply the learned overshoot offset`** — the controller
   latches `offset` at episode start and writes `requested − offset`, gated by
   §9.4's observe-only flag, **defaulted on**. Behaviour is therefore still
   unchanged after this commit; flipping the flag per zone is a deliberate
   separate act.
5. **`thermostat: reveal the commanded setpoint on tap`** — §8's disclosure in
   `thermostat.html`, plus §9.6's journal view, §9.6's JSON endpoint and
   §9.5's reset action.

## 12. Deferred

- **`enhanced_climate.lua` and the Lovelace card.** The children's room lives
  in `lib/zones.lua`, so `thermostat.lua` is the whole target. Extending it
  means §7's two sites plus card config, editor fields and a `VERSION` bump.
- **Bucketing `k` by conditions** (outdoor temperature, time of day). One
  scalar per zone assumes the plant gain is roughly constant across episodes of
  different sizes; proportional-to-rise handles the size, but not a mild day
  versus a cold one. Add buckets only if the measured error stays visibly
  correlated with the weather after `k` converges.
- **Cycle amplitude.** §2 — `heat_deadband` on the node, not here.
