# State: heating overshoot correction (overshoot-spec.md)

Spec: `overshoot-spec.md` (§5 is the current model). Global decisions — the
user's two absolute rules among them — live in `../AI.state`.

Status: **in `enhanced_climate.lua`, observe-only on all four climates.** The
current model ("heat on demand, cut on evidence", learned `c`) shipped in
v4.14.0; round 5's fixes (B1 turned-release, B2 strict cut) are in v4.15.0,
not yet deployed. Arming is the user's call.

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
- `radiator_entity`/`outdoor_entity` are set on nappali and gyerekszoba only;
  fürdő and háló episodes carry neither and so never cut.
- Replaying the recorder through the real lib puts c near 0.02 (first live run
  0.018).

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
  pulse, journaled `radiator_cold` with a warn. Expected.
- c measured at armed cuts sits above observe-only's 0.02 (the actuator keeps
  heating ~3 min after the relay opens). It climbs until cuts land under
  MIN_LEAD, where discards stop it: bounded, not a runaway.
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
- Tuning only if armed data asks: cut on radiator updates rather than the
  minute tick, a lower GAIN, requiring MIN_LEAD before an armed cut.
- At `log_level: debug` the log is a one-hour rolling window; the journal
  (SQLite) is the record.
