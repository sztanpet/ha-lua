# Heating overshoot correction — Specification

> **Working state:** [`state/overshoot.md`](state/overshoot.md) — implementation progress and decisions.

Status: **built for both controllers**, shipping in observe-only mode (§9.4).
§11's five commits landed in `thermostat.lua`; §12's port to
`enhanced_climate.lua` — which is where the children's room actually lives, and
was wrongly assumed not to be — is done too. See `state/overshoot.md`. Where
the code and this document disagreed, the document was corrected: the notes
saying so are kept deliberately, since each marks something that was got wrong
on paper first.

**Revised 2026-09-26** after the first evening of real data: the children's
room has no schedule. It is held at one temperature all day and overheats on
its own heating *cycles*, which the original trigger (§5, "the request rises
above the room") could not see at all — the request never moves during a hold.
An episode now also opens when the relay closes, and the offset gained a floor
term for exactly those cycles. §2, §4.2, §5 and §6 carry the change.

## 1. Goal

Stop small rooms from sailing past their setpoint on a warmup — a scheduled one
from setback, or a hold's own heating cycle — **without slowing the warmup
down**.

The motivating case is the children's room: an ESPHome node with a separate
room sensor, `climate: platform: thermostat` (bang-bang), driving a relay into
a thermal actuator on the radiator. It reaches its setpoint and then keeps
climbing 1–2 °C.

## 2. Non-goals

- **Not a replacement for the ESP's controller.** The node keeps
  `platform: thermostat` and keeps regulating against its own room sensor. All
  ha-lua does is choose the number it regulates *to*.
- **Not a swing/amplitude fix.** How far the room is allowed to fall *below*
  the setpoint before the relay closes is `heat_deadband` on the node, and a
  separate, cheaper lever. How far it sails *above* the setpoint once the relay
  opens again is this feature's whole subject, on a cycle as much as on a
  warmup from setback (revised: the first draft excluded cycles, see §4.2).
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

### 4.2 A flat learned offset, applied permanently

Fails in steady state. A *permanent* offset lowers the commanded setpoint for
good, so once the room is warm it holds around `requested − offset` rather than
`requested` — the room ends up simply too cold, all day, by exactly the amount
we cut. That objection stands, and it is why the offset is only ever applied
for the duration of an episode (§5): the command returns to `requested` when
the coast ends.

The first draft went further and claimed the big overshoot only happens on the
**first long run from setback**, short top-ups barely warming the radiator, so
a purely rise-proportional offset would do. **That premise was wrong.** The
children's room has no schedule; every run it makes is a "top-up" from the
deadband, and it overheats on every one of them. A relay that is on for ten
minutes brings the radiator to full temperature whether the room needed 0.3 °
or 3 °, and the stored energy it then dumps is much the same. The overshoot has
a floor that does not scale with the rise — so the offset has one too (§5),
learned per episode like the slope.

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

An **episode** opens on either of two triggers, checked on the controller's
1-minute tick with no episode already running:

1. the requested setpoint rises above the room temperature (a schedule
   transition, an override, a manual hold) — the warmup from setback; or
2. the device reports `hvac_action = heating` — the relay has closed for a run
   inside a flat hold. The request has not moved, so trigger 1 cannot see
   this, and it is the case the children's room actually lives in.

Either way, at that moment and only then, the controller latches:

```
rise    = requested − current                              -- how far it has to climb
offset  = clamp(k.base + k.slope * rise, 0, MAX_OFFSET)    -- both learned (§6)
command = requested − offset
```

`command` is held for the whole episode. The episode closes when the coast
window after the cutoff runs out, or when the requested setpoint changes again;
then the command returns to `requested`, so the offset never becomes the
permanent lowering §4.2 rejects.

`base` is the floor: the overshoot a run produces regardless of how far the
room had to climb, because the radiator reaches full temperature either way.
`slope` is the part that grows with a long warmup. On a cycle the rise is the
deadband — 0.2–0.5 ° — and the correction is essentially `base`; on an 18→21
warmup `slope` adds to it. A single rise-proportional `k` cannot serve both:
capped at `K_MAX * deadband` it could never cut more than ~0.4 ° off a cycle
that overshoots by 1.5.

On a cycle the correction can command a setpoint at or below the room, which
switches the relay straight back off. That is the intended limit case: the
learner then measures an undershoot, `base` comes down, and it settles where a
short run's stored heat lands the room exactly on the request. The room cycles
in its deadband instead of above it.

**The offset must be latched at episode start, not recomputed per tick.** If it
were recomputed, `command` would climb as the room warmed (`rise` shrinking
toward zero) and converge on `requested` without ever cutting early — the
correction would silently do nothing. This is the single easiest thing to get
wrong here.

With `base = 0` this collapses to the first draft's model exactly: an 18→21
warmup with `slope = 0.4` cuts at 19.8, a 20.7→21 top-up at 20.88. Both start
at zero (`K_INIT`), so a plant that has taught nothing is driven exactly as it
was before the feature existed.

This is **not** a proportional band. A P controller throttles output power as
it approaches; this holds full power and moves the stopping point. Fast *and*
early.

### 5.1 Constants

| Name | Default | Meaning |
|------|---------|---------|
| `K_INIT` | `{base 0, slope 0}` | Starting coefficients. Zero means the first episode behaves exactly as today — never worse than the status quo while it has learned nothing. |
| `GAIN` | `0.5` | Fraction of the measured error folded in per episode. Converges in ~4–5 episodes, damped enough not to ring. |
| `SLOPE_MAX` | `0.8` | Hard bound on `slope`. Sanity only; a plant needing more than this is broken elsewhere. `base` is bounded by `MAX_OFFSET`. |
| `MAX_OFFSET` | `2.5 °C` | Absolute cap on a single cutoff, whatever the coefficients say. |
| `COAST_WINDOW` | `30 min` | How long after cutoff the peak is watched for. |

## 6. The learner

Two numbers per zone, `k = {base, slope}`, in the script's KV store. After
each episode:

```
peak    = max room temperature observed within COAST_WINDOW of the cutoff
error   = peak − requested                      -- >0 too hot, <0 undershot
norm    = 1 + rise²
base    = clamp(base  + GAIN * error        / norm, 0, MAX_OFFSET)
slope   = clamp(slope + GAIN * error * rise / norm, 0, SLOPE_MAX)
```

This is one normalised gradient step (NLMS) on the regressor `[1, rise]`: the
error is split between the two coefficients in proportion to how much each
contributed to the offset that produced it. A cycle (`rise ≈ 0.3`) teaches
`base` almost entirely; a long warmup teaches both. The offset *at the
observed rise* moves by exactly `GAIN * error` per episode, which is the same
convergence rate the single-coefficient draft had — so nothing about "four or
five episodes" changes. No matrix, no memory of past regressors.

A discrete integral controller closed across days, either way. Because it
corrects on **measured outcome**, it tracks seasonal drift on its own: when a
milder month raises the plant gain and overshoot creeps back, the next few
episodes push the coefficients up without anyone retuning anything. That is
the direct answer to §4.1's brittleness.

The first draft also refused to learn from any rise under `MIN_RISE = 0.3 °`,
because dividing the error by a tiny rise blew the update up. The normalised
step has no such division, and cycles ARE small rises, so that gate is gone.
What replaces it is physical: an episode teaches nothing unless the relay was
actually seen on during it (`never_heated` below). A 0.2 ° nudge inside the
deadband never fires the heating, and whatever the room does afterwards is
weather, not the plant.

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
- the device never reported `hvac_action = heating` during the episode — the
  relay never closed, so there was no stored energy to measure. A device that
  reports no `hvac_action` at all is not gated (unknown is not "off")
- the room never reached the commanded setpoint within `MAX_EPISODE` (4 h) —
  the plant could not keep up, and nothing about overshoot can be read off it
- observe-only was switched for the zone mid-episode — the episode latched its
  setpoint from the old setting and cannot be judged against the new one
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

**The episode state machine lives in the controller too, not in a second
script.** An earlier commit plan put episode detection and peak tracking in an
`overshoot.lua` beside the controller. That splits one decision across two
scripts: the controller has to latch the offset at episode start (§5) and the
learner has to detect the same boundary to know when to start tracking the
peak, so both would re-derive it and drift. `store.*` is per-script, so `k` and
the journal would have to travel through `global` as well. The controller
already holds everything an episode needs — the request, the room temperature,
the mode, the window state, and a 1-minute tick. What stays separate is
`lib/overshoot.lua`: pure, no `ha.*`/`store.*`/`time.*`, unit-testable from Go.

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
              | "never_heated" | "restart" | "never_reached"
              | "observe_changed"
```

`never_reached` is the room failing to reach even the *reduced* setpoint within
`MAX_EPISODE` (4 h). Without it an episode that never cuts off sits open
forever and never learns — silent, and precisely §9.1's failure. It is also the
one discard that says something about the plant rather than about us.

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
| episode open | `info` | zone, requested, current, rise, base and slope, offset, commanded |
| episode close | `info` | peak, error, base and slope before → after |
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
- **As JSON:** `GET /api/overshoot?zone=<zone>` returns `k`, sample count, the
  live episode and the journal, so it is curl-able and greppable without the
  UI. (The path follows `thermostat.lua`'s existing `/api/...?zone=` shape
  rather than the `/zones/<zone>/…` this section first proposed.)
  `POST /api/overshoot/reset` and `POST /api/overshoot/observe` are the two
  writes §9.5 and §9.4 call for.

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
3. **`examples: add the heating overshoot learner`** — `lib/overshoot.lua`,
   pure and unwired: the latch, the clamp, the episode step function, the
   `k` update, and the `ok, reason` validity predicate of §9.2. Go unit tests
   alongside `lib/control.lua`'s. No behaviour change — nothing calls it yet.
4. **`thermostat: run the overshoot learner per zone`** — wire it into the
   controller: episode detection on the existing tick, peak tracking, `k` per
   zone, §9.1's journal, §9.3's log lines, and §9.4's observe-only flag
   **defaulted on**, so the commanded value is computed and recorded while the
   uncorrected setpoint is still what gets written. Behaviour is therefore
   still unchanged after this commit.
5. **`thermostat: reveal the commanded setpoint on tap`** — §8's disclosure in
   `thermostat.html` (which must first *add* a setpoint display), §9.6's
   journal view and JSON endpoint, §9.5's reset action, and the switch that
   takes a zone out of observe-only.

The learner and its diagnostics land together in commit 4, as §9 requires; what
moved out of it is only the pure library, because a pure module plus its tests
is the smaller bisectable unit and it keeps the wiring commit readable.

## 12. Deferred

- ~~**`enhanced_climate.lua` and the Lovelace card.**~~ **DONE** — and it was
  never optional. The premise recorded here, that the children's room lives in
  `lib/zones.lua`, was wrong and was never checked against the running install:
  the room is `climate.konyha_gyerekszoba_futes`, an enhanced climate, and the
  `thermostat.lua` on the box is an unmodified example copy pointing at entities
  that do not exist. The feature therefore sat in observe-only for three weeks
  having never opened a single episode. See `state/overshoot.md`, "Field check".

  The port brought two things this spec did not anticipate. The card's target
  stepper reads the setpoint off the climate entity, so it would have shown the
  COMMANDED value where the user set the request — and a nudge from a corrected
  19.8 lands inside `control.is_manual`'s 0.1 ° tolerance, so the tap would have
  appeared to do nothing at all. §8's "the shipped card does not read `target`"
  holds for `thermostat.html` only. And a live episode has to be abandoned when
  a climate stops being controlled entirely, which is a real case here (a boost
  expiring with no schedule under it) and not one in the zone model.
- **Varying the coefficients with conditions** (outdoor temperature, radiator
  temperature). `{base, slope}` assumes the plant gain is roughly constant
  across episodes; the slope handles the size of an episode, but not a mild
  day versus a cold one, nor a radiator that was already hot when the run
  started. Gate unchanged: do this only if the measured error stays visibly
  correlated with those columns after the coefficients converge. Note that §6
  is already a two-term NLMS on `[1, rise]`; a third regressor is one more
  line there, which is precisely why the fit below is the right shape and a
  lookup table is not.

  **The evidence is now being collected.** Every episode records
  `outdoor_at_open`, `radiator_at_open`, `radiator_at_cutoff` and
  `radiator_at_peak`, and the Ingress journal shows the first and third. This
  costs nothing and is what makes the question answerable at all — without the
  columns, "decide later from the data" is not a plan.

  **A lookup table keyed on outdoor temperature is the wrong shape**, and was
  rejected before any code. The data rate is about one episode per night per
  zone and `k` converges in four or five, so a single scalar is right within a
  week. Split into, say, nine 3 °C buckets across a season and each bucket needs
  its own four or five episodes — a month at best, and the shoulder buckets
  collect a handful all year. Worse, `K_INIT = 0` means every bucket the weather
  has not yet visited starts with the correction switched OFF: the overshoot
  would come back every time the weather moved into a new band, which is a
  regression triggered by precisely the thing the buckets were meant to handle.

  If the data justifies varying `k`, fit it instead:

  ```
  k = k0 + k1 * (T_out - T_ref)
  ```

  Two parameters by recursive least squares, both updated by every episode. No
  bucket can starve, it interpolates across the range and extrapolates past it,
  there is no discontinuity at a bucket edge, and there is no cold start ever.

  Note also which variable is which. The overshoot is stored energy in the
  radiator body — metal at 50–70 °C radiating for 10–20 minutes after the relay
  drops — and that quantity is set by the radiator's mass and water temperature,
  NOT by the outdoor temperature. Outdoor temperature governs how fast that
  energy leaks back out through the walls during the coast, so it is the
  second-order term despite being the obvious suspect. `radiator_at_cutoff` is
  the first-order one. If the boiler runs weather compensation the two become
  correlated, because the flow temperature then tracks the weather — which is
  exactly the sort of thing the journal will show and an argument will not.
- **Cycle amplitude.** §2 — `heat_deadband` on the node, not here.
