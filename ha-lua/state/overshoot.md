# State: heating overshoot correction (overshoot-spec.md)

Working state for the learned early-cutoff correction. Spec:
`overshoot-spec.md`. Global decisions live in `../AI.state`.

Status: **ported to `enhanced_climate.lua`, still observe-only.** The five §11
commits landed in `thermostat.lua`, which turned out to control nothing on the
live install ("Field check, 2026-09-26" below). The port to the controller that
actually owns the children's room is done — nine commits on 2026-09-26, listed
under "The port" — and ships observe-only, so the next step is still field data,
but now from a zone that will actually produce episodes.

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
- **The episode state machine lives in the controller, not a second script.**
  An earlier commit plan had an `overshoot.lua` beside the controller doing
  episode detection and peak tracking. That splits one decision in two: the
  controller must latch the offset at episode start and the learner must find
  the same boundary to start tracking the peak, so both re-derive it and drift.
  `store.*` is per-script, so `k` and the journal would travel through `global`
  too. The controller already holds the request, the room temperature, the
  mode, the window state and a 1-minute tick. `lib/overshoot.lua` is what stays
  separate: pure, no `ha.*`/`store.*`/`time.*`, testable from Go.
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
actually runs on. Payload splits `target` (requested) from `commanded`.

Checked against the code at commit 2, correcting an earlier claim in this file
and in spec §8: `thermostat.html` **does not read `target` at all**, and the
stepper edits `override_temp` (a KV value, untouched by the correction), so
there is no ratchet bug. What the check did turn up is bigger: the card has no
setpoint display whatsoever — head is room temp plus a status word, the only
number is the override temp. "Show the requested temp" therefore means adding
a number that has never been on the card, not relabelling one.

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

## Commits
1. `thermostat: publish the written setpoint separately` — `zones.written_key`,
   published every tick beside `desired`; `control.is_manual` and
   `heating_windows.lua`'s restore both moved onto it. Behaviour identical
   (`written == desired`). The two Go tests that cover the handoff now seed the
   two keys to *different* values, so they fail if either consumer drifts back
   onto `desired` — verified by reverting the change. `thermostat-ui-spec.md`
   §4.2 carries an amendment note; its original text describes the old
   single-key contract.

2. `thermostat: split requested and commanded in the zone payload` —
   `target` now computed from `desired()` (falling back to the device setpoint
   for a zone with no request at all), `commanded` added from the entity's
   `temperature` attribute. `commanded` is deliberately read *back from the
   device* rather than from `written`: it then also shows the window script's
   frost value and exposes a write that never landed. `TestThermostatAPI`
   forces the two apart via the override path (request 21, entity still 18,
   because its call_service is a no-op capture).

3. `examples: add the heating overshoot learner` — `lib/overshoot.lua`, pure
   and unwired, with Go unit tests. Two things the code forced back into the
   spec: a `never_reached` discard (an episode whose room never reaches the
   setpoint would otherwise sit open forever, learning nothing and saying
   nothing — exactly §9.1's silent failure), and the fact that **k's lower
   clamp is unreachable**. A run cuts off at `requested - k*rise`, so the peak
   cannot land more than the offset low and the update is bounded below by
   `-GAIN*k`: k halves toward zero and never crosses it. The zero clamp is
   defensive. Written down because the first test asserted it *was* reachable
   and was wrong.

4. `thermostat: run the overshoot learner per zone` — episode state machine on
   the existing tick, journal (ring of 50 per zone), log lines, `k`/`samples`
   per zone, observe-only defaulted on. Payload carries `k`, `samples`,
   `offset`, `observe_only` so commit 5 is pure frontend.

   Decisions taken while wiring:
   - **An episode opens on a REQUEST CHANGE to a value above the room**, not
     merely on the room sitting below setpoint — the latter is true on every
     tick of a normal hold and would open an episode a minute. `apply_zone`
     reads the previously published `desired` before overwriting it; that is
     the change detector.
   - **The commanded setpoint is clamped to the device's advertised range.** HA
     silently drops an out-of-range setpoint, so an unclamped command would
     never be applied and the episode would then wait forever for a cutoff that
     cannot arrive.
   - **An invalidated episode closes immediately** rather than limping to the
     end of its coast. The warn fires when the thing happens, and the
     correction stops when the reason for it stopped being true.
   - Test scaffolding: `writeThermostatScripts` now stages the example and all
     its libs in one place. Three copies of that list had already drifted — the
     new `require` broke the two this commit did not touch.

5. `thermostat: reveal the commanded setpoint on tap` — the card had no
   setpoint display, so one was added (room temp → requested temp in the head).
   Tapping it opens a panel with requested/commanded/offset, `k`, sample count,
   the observe-only switch, a reset, and the journal. Endpoints:
   `GET /api/overshoot?zone=`, `POST /api/overshoot/reset`,
   `POST /api/overshoot/observe` — the spec's `/zones/<zone>/overshoot` was
   changed to match this script's existing `/api/...?zone=` shape.
   Switching observe-only mid-episode closes the running one with a new
   `observe_changed` reason rather than dropping it silently. The journal is
   fetched on open rather than carried on `/api/state`, which is polled every
   5 s. A chromedp test pins that the panel is unreachable without a tap.

## Field check, 2026-09-26: the feature was built into the wrong script

Went looking for a week of journal data and there is none. Verified against
the running add-on (LAN port 8100 and ssh root@homeassistant):

- All three `thermostat.lua` zones report `k=0`, `samples=0`, `journal={}`.
  **Not one episode has opened** in the three weeks since v4.9.0.
- Because `thermostat.lua` controls nothing. `scripts/lib/zones.lua` on the
  box is **byte-identical to the bundled example** — so are `thermostat.lua`
  and `heating_windows.lua` — i.e. still the placeholder zones
  `climate.living_room` / `climate.bedroom` / `climate.kitchen`, none of which
  exist. Every zone reads `mode: "unknown"`. The real thermostat was moved to
  `scripts/disabled/` on 2026-07-04; unmodified example copies have been
  sitting in `scripts/` since, loading and ticking against dead entities.
- The children's room is `climate.konyha_gyerekszoba_futes`, registered in
  **`enhanced_climate.lua`** — the script §12 deliberately deferred. The live
  file contains zero occurrences of "overshoot" and has no `desired`/`written`
  split (only `desired_key`, `enhanced_climate.lua:69`).

So the premise this file and spec §12 rested on — "the children's room is a
`lib/zones.lua` zone, so `thermostat.lua` is the whole target" — is simply
wrong, and was never checked against the running install. Flipping observe-only
off would have changed nothing.

**Diagnostic gap worth fixing regardless:** §9.1 makes every *discard* a record
with a reason, but an episode that never *opens* leaves no trace at all — which
is exactly the silent failure the section was written to prevent, and it is the
failure that actually happened. A zone that has produced no episode in N days
needs to say so.

## The port to enhanced_climate.lua (2026-09-26)
Nine commits, each green on `make test`:

| commit | what |
|--------|------|
| `b1f9219` | `desired`/`written` split; `control.is_manual` moved onto `written` |
| `dc94213` | `commanded` in the companion, read back off the device |
| `ee8c0d8` | the learner itself, keyed by climate entity |
| `a78e384` | `/api/overshoot`, `/reset`, `/observe` + the card's `overshoot` command |
| `8a89781` | card stepper edits the REQUEST (0.3.34) |
| `16cfca2` | card tap disclosure (0.3.35) |
| `d25d7d5` | journal table on the Ingress page |
| `ae7fb24` | `lib/overshoot.lua` carries an optional env snapshot |
| `ce216b3` | `radiator_entity` reaches the daemon (0.3.36) |
| `e09d0ab` | outdoor + radiator recorded per episode (0.3.37) |

Decisions worth keeping:

- **The card's stepper was a real ratchet bug**, which spec §8 had dismissed
  after checking `thermostat.html`. This card DOES read the setpoint off the
  climate entity, so it would have shown 19.8 where the user set 21 — and worse
  than misreporting: a nudge from 19.8 gives 19.9, inside `is_manual`'s 0.1 °
  tolerance, so the dial change would not register and the next tick would put
  19.8 back. The tap would look broken. It now reads the companion's state
  (the request) whenever `controlled`, falling back to the device setpoint.
- **A live episode is abandoned when the climate stops being controlled.**
  A boost expiring with no schedule under it is a real case here and is not one
  in the zone model; without it the episode sits open until the 4-hour timeout.
- **Observe-only switching is detected in the step function**, not only at the
  command that flipped it, so an episode latched under the old setting cannot be
  judged under the new one however the flag was changed.
- **Removing a climate drops its k, journal and flags.** A re-added climate
  should not inherit a coefficient measured on a plant that may since have been
  replumbed.
- **`radiator_entity` moved into `configHash`**, reversing the v2.9.0 decision
  that deliberately kept it out. That decision was right while it was
  display-only; the learner needs the id daemon-side, so a change to it is now a
  real config change.
- **The outdoor sensor is card config, not a module constant.** A constant was
  written first and thrown away: this script's whole premise is that it is
  provisioned at runtime and has nothing to edit in the file, and a constant
  cannot be tested without patching it.
- **A missing sensor records nil, never 0.** A zero would read as a freezing
  radiator and poison exactly the analysis the columns exist for.
- The Ingress journal table was checked in headless Chromium at 420 px and
  760 px, which turned up two real layout bugs (flex items will not shrink below
  their content without `min-width: 0`, so the table pushed Remove off the row;
  and the status sentence pushed the arm/reset buttons off the edge).

## On bucketing k by outdoor temperature (asked 2026-09-26)
The user's instinct that outdoor temperature matters is right; a lookup table
keyed on it is the wrong shape. Full reasoning is now in spec §12. Short
version: ~1 episode/night and k converges in 4–5, so a single scalar is right
within a week while nine buckets need a month and the shoulder buckets starve —
and `K_INIT = 0` means an unvisited bucket ships with the correction OFF, so the
overshoot returns every time the weather moves into a new band. If the data
justifies it, fit `k = k0 + k1*(T_out - T_ref)` by recursive least squares
instead: nothing starves, it interpolates, no cold start.

Also worth remembering which variable is which: the overshoot is stored energy
in the radiator body, set by its mass and water temperature, so
`radiator_at_cutoff` is the first-order term and outdoor temperature (which only
governs the leak rate during the coast) is second-order — unless the boiler runs
weather compensation, which would correlate them. The journal will show it.

## Released
v4.11.0 (`c0d46a8`) carries the whole port; v4.12.0 (`655335a`) adds the coast
decay curve; v4.12.1 (`c05cd5d`) fixes the stepper lag the port introduced;
v4.13.0 adds the relay trigger and the floor term; v4.14.0 (`9a82774`) is the
radiator-gated redesign ("heat on demand, cut on evidence"), the coast-end
rules, the load-time abandonment fix and the fresh run counter.
What shipped is in `CHANGELOG.md`; not repeated here.

The decay work answers the user's 2026-09-26 question about measuring heat-up
and cool-down. Cool-down got the work because it IS the overshoot mechanism —
the heat landing in the room after the cutoff is the integral of that curve.
Heat-up needed no new storage: `opened_at`, `cutoff_at` and the two radiator
readings already bracket it. A HALF-LIFE, not a fitted time constant: no
regression, survives a missing sample, and does not pretend a slow valve decays
cleanly. nil when the lead never halved in the coast, because that is the
finding rather than something to paper over.

## The lag the port caused (v4.12.1)
The user reported "setting the setpoint on the card is now extremely laggy"
immediately after deploying. Two causes, both from commit 5 of the port:

1. The stepper started rendering the REQUEST (`companion.state`) instead of
   `attrs.temperature`. That is the correct number — while the correction cuts a
   warmup short the device carries `requested - offset` — but it swapped a
   one-hop source for HA event -> daemon -> `is_manual` -> `apply_climate` ->
   republish -> push back. `commit()` never wrote `input.value`, so a tap changed
   NOTHING on screen until that returned.
2. `radiator_entity` joined `_relevantChanged`, so the radiator sensor now
   rebuilds the whole shadow DOM. A rebuild carrying the pre-tap request reset
   the stepper's `lastSent`, which rewound the number AND made the next tap step
   from the rewound value. The stepper fought the user.

Fix (`4868d27`, card 0.3.38): echo the tap into `input.value` at once, and keep
`{value, from}` per stepper — `from` being the source value it was tapped away
from. A source still reading `from` has not caught up, so the echo wins; a source
that moved ANYWHERE ELSE is a real external change and outranks the echo. 8s
expiry as the backstop so a write that lands nowhere cannot leave the card lying.
Plus a 500ms debounce on the write: a TRV queues every `set_temperature`, so a
burst of taps made the setpoint walk to its destination after the tapping
stopped.

The lesson worth keeping: **moving a control's displayed value onto a
slower-answering source is a UI change, not just a data change.** Optimism-free
is right for server data and wrong for the number in a control the user is
touching — that number is their own input.

## The trigger that could not fire (v4.13.0)
Same evening, second report: "there was a heating cycle but i can't see
anything about the overshoot". Checked on the box: zero `overshoot` lines in
both log files (which cover 18:35 onward), empty journal. Not a logging
failure — nothing had happened. The request on the children's room had been a
flat 23.7 since a stepper nudge at 18:37 (a 24h manual hold; the room has NO
schedule), and the only trigger was "the request rises above the room". A
cycle inside a hold never moves the request. The model was built for a warmup
from setback in a room that does not exist here.

Two side findings while looking:
- The add-on runs at `log_level: debug`, and at debug the daemon writes a
  `lua: event dispatch delay` line for every event to every script — 12,427
  lines in 25 minutes, so the 5 MiB budget rotates in about an hour. The log
  is a rolling one-hour window, not a record; the journal (SQLite) is the
  record. Told the user; the per-event debug line itself was left alone.
- `overshoot.open` returning nil for "nothing to climb" is silent. The two
  0.1° nudges at 18:37 produced no episode and no line. Still silent; the
  "no episodes in N days" warning has now been offered four times and not
  asked for.

The fix, spec first (`781ba91`) then four commits: `{base, slope}` learned
by NLMS on `[1, rise]` (`389c53f`), the relay trigger with the handlers
reordered so manual detection runs before the re-apply (`3139ff6`), the card
(`0.3.39`). §4.2's rejection of a flat offset was kept for the PERMANENT case
and corrected on its premise: the offset is only ever applied for the episode.
`MIN_RISE` is gone (it would have discarded every cycle), replaced by
`never_heated`, which is gated only on an explicit hvac_action = idle so a
device without hvac_action is not locked out.

What to watch for once it has data: on a cycle the correction can command a
setpoint at or below the room and the relay switches straight back off — by
design; `base` then comes down until a short run lands the room on the
request. If `base` converges to something near the deadband and the room
still overshoots, the radiator's stored heat is more than a run of any length
can avoid and the answer is on the node (`heat_deadband`), not here.

## The first real data, and what it broke (2026-09-27/28)
One observed cycle (times in this section are UTC, the box is +02:00; Sat 20:51, deployed v4.13.0): relay-triggered, room 23.5 →
cut 23.7 after 13½ min → peak 24.1, radiator 23.5 → 45.5 and still rising a
minute past the cutoff (actuator lag, measured), lead half-life 27.8 min, room
still AT its peak when the fixed 30-min coast closed. That drove `5ee170d`: the
coast now ends on the room turning (0.2 below a peak settled for 5 min), on the
relay closing again, or a 90-min backstop. `e12fd8e`: `record()` had never
carried `heated`, so the journal said nil and I wrongly told the user the
device lacks `hvac_action` — it has it (read off the recorder DB).

**Then the user armed it (Sun ~18:00–22:47), and the journal showed the design
is wrong for cycles.** From 22:47 every ~31 min: relay closes at 23.3 (request
23.4) → episode opens with offset ~0.6 → command ~22.8 → the ESP stops after
`min_heating_run_time` (60 s) → radiator never warms (22.4–22.9 at "cutoff",
BELOW the room) → 30-min coast → close → command back → relay closes again.
`base` walked 0.63 → 0.38 on stubs, then a real run bounced it back up.

Why, from ESPHome 2026.9.0 source (`heating_required_()`): the switch-on test
`current <= target - deadband` runs FIRST, and `heat_overrun: -0.2` ≤ −deadband
makes the hysteresis branch unreachable, so the node is a pure threshold. When
the relay closes the room is AT the threshold; any offset larger than the
deadband puts it above the new one and cancels the run. On a threshold node a
correction latched at relay-close can only do nothing or cancel. And no
per-run cut can land a cycle's peak on the request anyway: peak ≥ start + the
coast of the shortest run, so the run must START below the request.

Also found: **observe-only learning diverges.** "observed" episodes fold the
uncorrected error in, which cannot respond to the coefficients — an open-loop
integrator. `base` climbed 0 → 0.19 → 0.33 → 0.38 → 0.63 over four observed
cycles; it would have reached MAX_OFFSET. This bug predates the port (the
original single-`k` design has it too).

**Proposed redesign (awaiting the user):** `base` becomes a HOLD-LONG offset
(command `requested - base` for the whole hold, so the request becomes the
ceiling of the swing — the average drops by about `base`), learned from
relay-triggered cycles as `base += GAIN * err`; `slope * rise` stays an
episode-latched extra for request-change warmups only, where the room is far
below the cutoff and a per-run cut works. Observe-only learns against the
counterfactual `peak - offset it would have applied`, which a whole-swing shift
makes one-for-one. Then reset learning — everything journaled so far is either
open-loop or stubs. Told the user to set observe-only until then.

Other facts from the same session: ESPHome defaults `heat_deadband` and
`heat_overrun` to 0.5 (schema, 2026.9.0), so "ditching" them is a one-degree
band, not a simplification; explicit 0/0 is the simple threshold (heat iff
room ≤ target) and moves this room's threshold up 0.1 from today's. All four
zones live in `/config/esphome/konyha.yaml` (a git repo); the fourth zone is
already 0/0, háló is 0.05/−0.2. Heating-season automation
(`automations.yaml` id 1782250376702) changed on the box 2026-09-28 at the
user's request: on below 14 (was 14.5), off above 17 (unchanged).

## The redesign: heat on demand, cut on evidence (2026-09-28)
The user rejected the hold-long offset (it starts every run later) and gave
the rules instead, verbatim: "it must start whenever the temp is below the
setpoint ... whenever you see the radiator increasing in temp, you can start
thinking about switching it off, but never before". Spec §5/§6 rewritten
(`e47f2d1`), implemented in `1a58fb1`, card 0.3.40 (`bbf839f`).

- A run's episode opens on relay-close (or request rising above the room for a
  device without hvac_action) and writes NOTHING.
- Gate: radiator ≥ its lowest reading in the run + `RAD_RISE` (1.0 °C). A
  radiator still warm from the last run is not evidence about this one.
- Cut when `room + c * max(0, radiator - room) >= requested`; the hold is
  `room - HOLD_MARGIN` (0.5), clamped to the device range and never above the
  request. Released the moment the prediction falls short (rule 1), or with
  the coast.
- `c` is measured per run at whichever cutoff happened (correction's or the
  node's own relay-open) as `(coast peak - room at cut) / lead at cut`,
  smoothed with GAIN 0.5, clamped [0, C_MAX 0.2]. Observe-only converges on it
  (mutation-tested: the old integrate-the-error rule fails the test). The
  first live run gives c ≈ 0.018.
- New KV key `overshoot_c:<climate>`; the old `overshoot_k:` ({base, slope})
  is ignored, not deleted. The coast's peak restarts at the cutoff.
- No radiator reading → never cuts (no evidence). thermostat.lua reads the
  radiator its zones already list for valve_watch.
- Found while doing it: enhanced_climate.lua never abandoned a live episode at
  load (thermostat.lua always did; the v4.11 port missed it). Fixed on its own
  in `2039a49`, before the redesign, since a hold must not outlive a restart.
- New discard reasons: `no_radiator`, `radiator_cold` (lead < MIN_LEAD 3 °C);
  `rise_too_small` is gone.

Same day, at the user's request or with their go-ahead:
- Children's room switched back to observe-only at ~08:30 local (it was
  cancelling runs); all four climates now observe-only.
- `/config/esphome/konyha.yaml`: heat_deadband/heat_overrun 0/0 on nappali,
  gyerekszoba, háló (fürdő already was), committed in the esphome repo as
  `a31d8aa` authored as the user. NOT FLASHED — the user flashes from the
  ESPHome dashboard.
- Heating-season automation: the recorder (read with sqlite3 installed
  ephemerally in the SSH add-on; automation runs are excluded from the
  recorder, but climate mode changes carry a context) showed it switched ON
  Tue Sep 22 16:56 (24 h mean 14.49) and OFF Sat Sep 26 23:51 (17.01, twice
  in 16 s as the mean jittered across 17.00); the user then flipped zones by
  hand eleven times on Sunday. 14/17 would have skipped the ON but not the
  OFF. The helper uses `mean` (sample mean), 0.1–0.4° above a time-weighted
  mean — minor. The real problem is timing: a rolling 24 h mean crosses at any
  hour, and it switched heating off just before an 11° night. DONE at the
  user's go-ahead (09:28 local): on the moment the mean drops below 14
  (numeric_state), off only at 10:00 and only if the mean is then above 17
  (time trigger + condition). Validated with the Supervisor's config check
  before the reload. The mean was 18.1, so it switched everything off at 10:00
  the same day. Note for the user: on Sunday they turned heating back on at
  06:22 with the mean at ~17.2, which suggests 17 is low for this house.

## Pending
- **Deploy v4.14.0** (user): update the add-on (card 0.3.40), re-copy
  `enhanced_climate.lua` and `thermostat.lua` (the run-counter key changed),
  and RESTART — a hot reload of the scripts does not pick up lib/ changes. The
  user deployed the redesign's scripts by hand before this release with a hot
  reload only; I forced a second reload at 09:40 by rewriting the two scripts
  unchanged, so the new lib was loaded for certain.
- DONE: konyha.yaml flashed (firmware built 08:49:30, after the edit).
- DONE: the heating-season automation uses `sensor.kinti_3_napos_atlag`, a UI
  statistics helper the user created (average_step, 72 h; matched an
  independent computation 16.95 vs 16.96). Paused 09:42 → ~11:45 so the 10:00
  check would not switch off on the 24 h mean the user was replacing.
- Open question to the user: "Fürdő fűtés reggelente" still conditions the
  morning bathroom boost on the 24 h mean < 17.
- Then watch the journal's would-cut rows in observe-only before arming.
- Whether 17 is the right switch-off threshold (see the Sunday note above).
- **Deploy.** The scripts on the box are hand-copied into
  `/config/ha-lua/scripts/`, and were byte-identical to the bundled examples, so
  none of this reaches the children's room until they are re-copied. The card
  also needs a new image plus a restart to re-materialize (0.3.38).
- DONE 2026-09-26: scripts re-copied (mtime 18:29) and `radiator_entity` /
  `outdoor_entity` set on the nappali and gyerekszoba climates — confirmed live
  via `/s/enhanced_climate/api/list`. Fürdő and Háló still have neither, so
  their episodes will carry no radiator or outdoor readings.
  `sensor.kinti_atlagos_napi_homerseklet` is the house's daily-average outdoor
  sensor (it drives the heat/off automation: 14.5/17 °, changed to 14/17 on
  2026-09-28).
- ~~Field data, then arm.~~ WRONG ADVICE, given 2026-09-26: observe-only cannot
  converge (open loop), and the cycle correction cannot work on this node. The
  user followed it and armed on Sunday. See the section above.
- The 23.7 manual hold from 18:37 pins the room until it expires (no
  schedule → 24h); the user was told.
- Still open: the "no episodes in N days" warning. A discard is journaled with a
  reason, but an episode that never OPENS leaves no trace — which is the failure
  that actually happened here.
- The example `thermostat.lua`/`heating_windows.lua` copies squatting in
  `scripts/` were left alone on the user's instruction (2026-09-26: "dont delete
  anything"). They were never customised; the real versions are in `disabled/`.
- A CHANGELOG entry and a release, when asked.
- Commit 5 was larger than the spec first implied: the card has no setpoint
  display to hang the disclosure off, so one had to be added.
