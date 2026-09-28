# State: bundled reference examples (load-examples-spec.md)

Spec: `load-examples-spec.md`. Global decisions live in `../AI.state`. The
heating controllers have their own files (`thermostat-ui.md`,
`enhanced-climate.md`, `overshoot.md`).

Status: **COMPLETE** (2.2.0); examples keep growing on top.

## The tree
- `examples/` is REFERENCE-ONLY: embedded, Materialized read-only to
  `/config/ha-lua/examples` on every boot (before the HA seed wait, so it
  appears even with HA down), never loaded. A runnable second script source
  was designed and rejected as needless complexity.
- Some examples carry the author's REAL entity ids on purpose
  (mirrored_switches, ikea_dimmer, nappali_switches, group_switches).
- **Entity ids are ASKED, never derived.** The author's ZBMINIR2 relays are
  `switch.zbminir2_*` in some places and `light.zbminir2_*` in others (a domain
  override on top of Z2M discovery); a guessed prefix ships a script that
  silently never fires. Scripts warn at load on an unknown id.
- gopher-lua is Lua 5.1: no `goto continue`.

## mirrored_switches.lua
- Calls `ha.immediate_events()`: the 100 ms batch window was the whole "slower
  than a built-in automation" gap. That is the first thing to check whenever a
  script feels slow; the docs say so loudly.
- Echo attribution, not state comparison (see `event-latency.md`). Now a
  special case of group_switches, but kept: README, DOCS and lua_api.md use it
  as the teaching example.

## battery_levels.lua
- The ETA cannot come from `ha.get_history` (2-day retention); the script keeps
  its own series, one sample per observed level change, in its KV store.
- **Drain rate = Theil-Sen median with a 12 h minimum pair span.** Chosen by
  measuring variance on real data (`wobbleSeries()` in the test is REAL user
  data, a ~0.25 %/day drain under a ±1 diurnal swing — do not tidy it). The
  secant swung 2.6×, plain Theil-Sen returned exactly 0 (19 of 55 pairs had
  Δlevel 0), least squares was biased. **`MIN_PAIR_SPAN` is load-bearing.**
  Appending `(now, level)` as a point does NOT handle a stall; `dwell_cap` does.
- `RECHARGE_RISE = 10` above the run's LOW. A per-step threshold wiped noisy
  series constantly and missed slow top-ups. Do not lower it.
- `MIN_SPAN` 24 h; a battery that never stepped gets a floor (`eta_at_least`)
  that never outranks a measurement in sorting, since it is systematically
  pessimistic. `EMPTY_LEVEL` 15 (user request).
- Ignored batteries stay listed, dimmed and last, and are not sampled; their
  series is deleted so tracking resumes fresh.
- Series cleanup only when the entity is gone from the mirror entirely, never
  when it is unavailable.
- The `events:<entity>` trail and `/api/detail` exist to diagnose ETA swings;
  `/api/detail` is read-only on purpose (inspecting must not sample).
- Rejected (user said no): seeding from HA's long-term statistics.

## service_api.lua
- One endpoint calls ANY service; everything outside the reserved keys is
  forwarded verbatim. Text transports turn a value into a number ONLY when
  `tostring(tonumber(v)) == v` (an alarm `code=0123` stays a string);
  `entity_id` alone is comma-split. The page's JS `typeOf` mirrors the Lua
  `coerce`, so change both together.
- **The page embeds the token when it serves it — reversed on the user's
  instruction.** Anyone who can open the page on the LAN port has the token;
  the user accepted that. Do NOT "fix" it back to a prompt. The injection
  escapes `\`, `"` and `/` and substitutes through a gsub FUNCTION.
- Service names cannot be enumerated (no binding for HA's registry), so the
  builder's list is a suggestion, not a whitelist.

## ikea_dimmer.lua
- Input is MQTT (`zigbee2mqtt/ikea dimmer 1/action`); the device trigger is
  invisible to the WS API. The ramp runs in the daemon as an `ha.after` chain.
- No timer-cancel API: a generation counter disarms stale steps. Do not add
  cancellation to the scheduler for one example.
- Seed the ramp from our own last commanded level while it is <5 s old; the
  reported brightness lags.
- `ha.on_event` handlers get the event's DATA table, not the envelope.

## nappali_switches.lua
- Two Aqara rockers over MQTT: single click owns one light, double/hold takes
  both. "Both" is a GROUP toggle (any on → all off), a judgment call, not asked.
- The action topic is not retained, so a reload cannot replay a press.
- Unverified: whether the device ever emits `single` ahead of `double`. If a
  double toggles one light then both, add a ~350 ms single-click debounce.

## group_switches.lua
Any press drives every entity in the group to one state. The design
constraints, each learned from a field report:
- Switches can also be lamps (a wall switch driving its own relay); the dual
  role is keyed off list membership only.
- Echo attribution, or the room strobes; no queue entry for the pressed entity
  itself, or a phantom swallows the next press.
- **Direction inverts the group's aggregate as it stood just before the
  press**, counting a pressed dual-role switch at its OLD state. Direction from
  the pressed switch's own state was wrong whenever anything had drifted.
- Everything listed is commanded, pure inputs included, but only LAMPS vote in
  "is the room lit".
- `homeassistant.turn_on/off`, not `light.*`: a light call silently skips a
  relay in the switch domain.
- `last_command` wins for 5 s over reported state (two presses inside the round
  trip); an uncommanded report from a non-switch lamp voids it.
  `FOLLOW_OUTSIDE_CHANGE` (default on) lets an outside lamp change drag the
  group.
- Watch for: a relay in Z2M's following mode with a maintained lever may snap
  back after our command and read as a press. Detached mode fixes it; else a
  revert guard.
