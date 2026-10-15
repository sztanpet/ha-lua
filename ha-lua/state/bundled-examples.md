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
  (mirrored_switches, ikea_dimmer, nappali_switches, group_switches,
  heating_season).
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

## heating_season.lua (2026-10-10, `019a44c`, deployed 10:52 CEST)
- Replaces the HA automation "Climate: Toggle Heating via Daily Outdoor
  Average" (`automations.yaml` id 1782250376702). The user asked for it in
  Lua, NOT in HA. Removed from `automations.yaml` on 2026-10-10 at the
  user's request (core/check ok; backup `automations.yaml.bak-heating-season`
  on the box). The user reloaded automations the same day (the SSH add-on
  cannot: 401). Proof it is gone: the zones stay in heat past 10:00 on
  10-11 with the mean above 17.
- Why: from 10-08 the 3-day mean sat at 17.3–18.7 with cold nights, so the
  automation switched heating off every 10:00 and the user turned it back on
  by hand every evening.
- Rule (user's choice): at most ONE switch-off per season. The first reading
  below 14 sends heat and arms it (`store` key `off_armed`); the next 10:00
  above 17 spends it. A hand-set mode then stands until below 14 again.
  Re-sending heat on each later dip below 14 was dropped on purpose, so a
  winter manual "off" is not overridden.
- Declined by the user: anti-short-cycle guards (minimum run / minimum off,
  in ha-lua or on the ESP). Boiler runs are 2–2½ min since the overshoot cut
  moved to detection; the user does not want that limited.
- Known gap: the scheduler catches up a missed 10:00 at load, so a daemon
  down at 10:00 on the armed day switches off whenever it comes back, even in
  the evening. Accepted as rare (one armed morning a year).
- Started unarmed at a mean of 18.7: nothing happens until it drops below 14.

## halo_pc_tv.lua (2026-10-12, `3e0161a`, deployed 20:29 CEST)
- `media_player.bedroom_tv` (androidtv_remote) follows the bedroom PC (HALOPC,
  192.168.1.12). Online = HA Ping integration, added by the user in the UI
  (the add-on can't create config entries): `binary_sensor.bedroom_halo_pc`,
  count 3, consider_home 30 s.
- Why Ping: nothing on the box reported the PC — `device_tracker.halopc`
  (Mikrotik) is disabled, and MELKOR's hass-workstation-service entities have
  been `unavailable` for the whole recorder history. ha-lua has no ping.
- No off delay in the script (user: Ping's consider_home is enough). A 2 min
  OFF_AFTER was written first and dropped. WRONG premise (found 10-13):
  consider_home only feeds the (disabled) Ping device_tracker; the binary
  sensor is the last round's is_alive, so one round with every ping lost
  turns the TV off. Poll interval is hardcoded 30 s; `update_entity` can force
  more but the coordinator debouncer caps it at one round per 10 s. User set
  count 2, consider_home 10 s (10-13) — the latter has no effect on the
  sensor. Faster-than-poll option offered: PC pushes on/off to ha.serve from a
  Windows Scheduled Task.
- Retry (user asked): drive until the TV REPORTS the state, 5 tries 20 s
  apart, `pcall` around the call. androidtv_remote (HA 2026.10.1 source):
  state on/off from the TV's push, `unavailable` when disconnected, and the
  key send raises then. turn_on/turn_off both send POWER, skipped by HA if
  `is_on` already matches — so a retry faster than the state push toggles the
  TV back. A generation counter drops the old chain on a new PC edge.
- Ping works: the sensor went off->on cleanly at 20:37:54 on 10-12.
- BLOCKED on the TV (10-12): `media_player.bedroom_tv` goes `unavailable`
  ~16 s after switching off (TCL Smart TV Pro drops off the network in
  standby). HA silently skips unavailable entities in a service call, so all
  5 tries "succeeded" and the script gave up at 20:39:35. WoL is no way round
  it yet: `wake_on_lan.send_magic_packet` is not registered on this install
  (the WoL switches are YAML switch-platform), and the SSH add-on is not on
  the LAN. WoL from the dev box (192.168.1.139, on the LAN) did not wake it
  either: the NIC answers ARP in standby, but no service is up.
- UNBLOCKED (10-12 20:52): the user changed a TV setting so it stays connected
  in standby (reports `off`, not `unavailable`). Via HA `media_player.turn_on`
  the TV reported `on` in ~4 s; `turn_off` -> `off` in ~2 s and still `off`
  (reachable) 30 s later. The PC->TV edge itself not yet seen end to end.
- REPLACED Ping with an in-script probe (10-15): new stdlib `net.dial(addr
  [, timeout])` (`5e35e02`; TCP connect + close, default 1 s, cap 5 s since it
  blocks the script goroutine; no raw socket needed in the add-on). Script
  (`7426f3f`) dials 192.168.1.12:445 every 2 s, 250 ms deadline (user: short
  deadline; PC RTT 0.78 ms, SYN resent only after 1 s). 1 answer = online,
  3 misses in a row = offline; first verdict after load is not an edge. PC
  had 135/139/445/3389/7680 open on 10-15. Cost: one timers-row UPDATE per
  fire (every 2 s). Released in v4.17.0 (`5d6f212`, 10-15). Add-on updated
  to 4.17.0 by Claude via the Supervisor API from the SSH add-on
  (`POST /store/reload`, then `POST /store/addons/0f182187_ha-lua/update`
  with `{"backup":false}`), script deployed 17:38 CEST, loaded with 1 timer.
  From the add-on container: 192.168.1.12:445 answers in ~0.6 ms, a dead
  address times out at exactly 250 ms. The PC->TV edge itself not yet seen
  end to end. The Ping config entry is now unused (user may delete it).
- RENAMED (10-15, user: "dial" is a bad name for a yes/no): `net.dial` ->
  `net.reachable`, returning `true` | `false, err` (a predicate, not the
  I/O modules' nil, err). Released as v5.0.0 (`3d3eed4`; user chose the
  major bump over keeping a `net.dial` alias). The add-on update and the
  script copy must go together: 5.0.0 breaks the old script, 4.17.0 the new.
