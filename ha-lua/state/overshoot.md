# State: heating overshoot correction (overshoot-spec.md)

Spec: `overshoot-spec.md` (§5 is the current model). Global decisions — the
user's two absolute rules among them — live in `../AI.state`.

Status: **in `enhanced_climate.lua`, v4.15.0 deployed. The children's room is
ARMED** (by the user from the card, evening of 2026-10-01); the other three are
observe-only. Peak-lead `c` (`5ea6be9`) is committed, not released.

## The plant (children's room)
- `climate.konyha_gyerekszoba_futes`, no schedule. ESPHome
  `climate: platform: thermostat` in `/config/esphome/konyha.yaml` (a git repo;
  all four zones live there), relay → HA → radiator node → wax actuator.
- **The node is a pure threshold.** ESPHome tests switch-on
  (`current <= target - deadband`) first, and with overrun ≤ −deadband the
  hysteresis branch is unreachable. Nappali, gyerekszoba and háló were set to
  deadband/overrun 0/0 on 2026-09-28 (esphome repo `a31d8aa`) and flashed;
  fürdő already was. ESPHome's defaults are 0.5/0.5, not 0.
- Sensors and lags: the `childrens-room-instrumentation` memory. The room
  sensor reports only ≥0.2 °C moves, so journal peaks move in 0.2 steps until
  D1 (`code-review.md`, round 5) lands.
- `radiator_entity` is set on nappali, gyerekszoba and háló; fürdő has none and
  so never cuts. Nappali/háló/fürdő have learned nothing: every run is a 30°
  request changed before it closed (`setpoint_changed`).
- **One central boiler and circulation pump** (user, 2026-10-02): the pump runs
  while any zone demands heat and stops at once when the last drops; the wax
  valve then takes ~5 min to close. No pump overrun. The radiator sensor is
  strapped to the radiator's surface, not in the water: at 10-02 02:14 CEST,
  every other zone off/idle, it climbed 26.8 → 43° in the 6 min after the
  relay opened. That is the sensor catching up with water already delivered,
  so by the time rule 2's gate sees +1° the radiator is largely full.
- Journal stamps are epoch; the box is CEST, the dev machine UTC.
- Replaying the recorder through the real lib puts c near 0.02 (first live run
  0.018).

## First armed night (2026-10-01/02)
- Every armed cut landed at the gate, ~5 min in: runs start with the room ON
  the request (deadband 0), so any positive `c × lead` passes it.
- The minimum dose (relay to the gate + actuator close lag) is the whole
  overshoot: a gate cut still peaked +0.6, like the node's own 11–12 min runs.
  No `c` shrinks that under rule 2; only runs starting well below the request
  can gain. The user declined to revisit the deadband.
- `c` on the cut lead read 0.16 and jumped 0.009 → 0.084 in one run. Now
  divided by the coast's peak lead (`5ea6be9`): armed cuts read 0.03–0.04,
  observe-only 0.025. The live `c` stays 0.084 until runs pull it down (GAIN
  halves the error each) or the user resets it.

## Decisions not to re-litigate
- **The ESP keeps `platform: thermostat`.** PID + `slow_pwm` was the textbook
  answer and was rejected: it costs warmup speed by construction, fixed gains
  drift with the seasonal plant, and this disturbance repeats nightly. It would
  also have broken valve_watch (`hvac_action` churning every period).
- **A setpoint correction can never be applied at the moment the relay
  closes.** On a threshold node the room is AT the threshold then, so any
  offset cancels the run after `min_heating_run_time`. Armed on v4.13 that was
  a 60-second relay pulse every 31 min into a radiator that never warmed.
- **An integral learner must not learn open-loop.** v4.13's observe-only
  folded the uncorrected error into its coefficients, which nothing could make
  respond, and `base` climbed towards the clamp. `c` is now a smoothed
  MEASUREMENT (coast rise / radiator lead at the cutoff), which converges the
  same whether observing or armed.
- **The trigger is the relay, not the request.** A room with no schedule never
  changes its request, so request-change-only episodes never opened there.
- **A hold-long offset was proposed and rejected** by the user: it starts every
  run later (rule 1).
- A cut is decided on the radiator: none configured means never cut. A missing
  sensor reading is recorded as nil, never 0.
- The episode state machine lives in the controller, not a second script: both
  would have to find the same run boundary, and `store.*` is per-script.
  `lib/overshoot.lua` stays pure and Go-testable.
- Observe-only ships ON; arming is a deliberate per-climate act. `c` resets
  from the card or the Ingress page (with a confirm since v4.15.0), never via
  sqlite or a restart. An episode in flight at load is abandoned and journaled.
- One `c` per room, not a table bucketed by outdoor temperature: ~1 run a night
  starves the buckets. If the journal ever shows outdoor dependence, fit
  `c = c0 + c1·(T_out − T_ref)` by recursive least squares instead. The radiator
  lead is the first-order term; outdoor is second-order unless the boiler runs
  weather compensation.
- The old `overshoot_k:` keys ({base, slope}, v4.13) are ignored, not deleted.

## Expect once armed (from the replay — not bugs)
- Runs starting 0.1–0.2 below the request are cut 1–3 ticks after the radiator
  starts rising, 7–11 min into runs the node ran for 12–22 min.
- A run starting with the reading exactly on the request is a ~5–6 min relay
  pulse. Expected; since the peak-lead fix it is learned, not `radiator_cold`.
- Watch `released_by`, the lowest reading before the next run (no more than a
  sensor step under the request) and relay cycles per hour.

## House automations (on the box, changed at the user's request)
- Heating season (`automations.yaml` id 1782250376702): on below 14, off at
  10:00 only if `sensor.kinti_3_napos_atlag` (a 72 h average_step statistics
  helper) is above 17. A rolling 24 h mean had switched heating off just before
  an 11° night. Whether 17 is right is open (the user turned heating back on at
  a mean of ~17.2).
- "Fürdő fűtés reggelente" keeps the 24 h mean (< 17). Asked; the user said no
  to changing it.

## Open
- The "no episodes in N days" warning: offered repeatedly, never asked for.
- **The dose is detection time.** On a gate cut hot water flows ~90 s before
  the relay drops; the radiator node's 60 s averaged report (30–90 s late)
  plus the 1-min tick were most of it. Fixed on the node side: esphome repo
  `5ba4fc9` (`.radiator.yaml`, all three radiator nodes) sends a raw reading
  every 60 s and at once on a 0.3° move. NOT FLASHED yet (the user flashes
  from the ESPHome dashboard). Next: cut on radiator updates rather than the
  minute tick (offered, not asked for yet).
- Faster actuator: Oventrop Aktor M (M30×1.5, ~3 s, 3-wire, power on brown
  CLOSES, so brown goes on the relay's NC contact). Helps the 4-min opening
  lag and overlap mornings; not a night alone, where the pump stopping
  already ends the flow. The user's call; not bought.
- Tuning only if armed data asks: a lower GAIN, requiring MIN_LEAD before an
  armed cut.
- At `log_level: debug` the log is a one-hour rolling window; the journal
  (SQLite) is the record.
