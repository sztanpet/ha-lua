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
decay curve. What shipped is in `CHANGELOG.md`; not repeated here.

The decay work answers the user's 2026-09-26 question about measuring heat-up
and cool-down. Cool-down got the work because it IS the overshoot mechanism —
the heat landing in the room after the cutoff is the integral of that curve.
Heat-up needed no new storage: `opened_at`, `cutoff_at` and the two radiator
readings already bracket it. A HALF-LIFE, not a fitted time constant: no
regression, survives a missing sample, and does not pretend a slow valve decays
cleanly. nil when the lead never halved in the coast, because that is the
finding rather than something to paper over.

## Pending
- **Deploy.** The scripts on the box are hand-copied into
  `/config/ha-lua/scripts/`, and were byte-identical to the bundled examples, so
  none of this reaches the children's room until they are re-copied. The card
  also needs a new image plus a restart to re-materialize (0.3.37).
- Set `radiator_entity` and `outdoor_entity` on the four enhanced-climate cards.
  `sensor.kinti_atlagos_napi_homerseklet` is the house's daily-average outdoor
  sensor (it already drives the heat/off automation at 14.5/17 °).
- Field data: a week of `/api/overshoot?climate=climate.konyha_gyerekszoba_futes`
  or the Ingress panel, THEN take that climate out of observe-only.
- Still open: the "no episodes in N days" warning. A discard is journaled with a
  reason, but an episode that never OPENS leaves no trace — which is the failure
  that actually happened here.
- The example `thermostat.lua`/`heating_windows.lua` copies squatting in
  `scripts/` were left alone on the user's instruction (2026-09-26: "dont delete
  anything"). They were never customised; the real versions are in `disabled/`.
- A CHANGELOG entry and a release, when asked.
- Commit 5 was larger than the spec first implied: the card has no setpoint
  display to hang the disclosure off, so one had to be added.
