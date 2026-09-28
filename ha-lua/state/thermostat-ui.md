# State: thermostat UI + heating examples (thermostat-ui-spec.md)

Working state for the thermostat web UI and the heating example scripts that
share `examples/lib/zones.lua` (thermostat, heating_windows, valve_watch).
Spec: `thermostat-ui-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE** (milestones 1–7, released 1.1.0 → 2.6.0, tweaks since).
**Not live on the user's box:** the copies in `scripts/` there are unmodified
examples aimed at placeholder entities; the real heating is
`enhanced_climate.lua`. Leave those copies alone (user, 2026-09-26).

**Vocabulary** (2026-06-23 rename): the UI's timed "boost" is now "override",
and the old dial-detected "override" is now "manual". Specs and CHANGELOG
entries older than 2.6.0 use the old words.

## Decisions and why
- **HA silently drops a `set_temperature` outside the entity's
  `min_temp`/`max_temp`.** Every setpoint, override temp and schedule entry is
  clamped to the device range. This is also why `call_service` waits for HA's
  result frame (since 2.3.0): the drop used to leave no trace.
- Manual-change detection compares against `written` (what was commanded),
  with a 0.075 tolerance since v4.15.0. Open or unseeded windows and an active
  override suppress it.
- The manual hold's store field is `expires`, not `until` (Lua keyword).
- `schedule.lua` is pure, so it is Go-unit-testable; weekday is
  `lua_dow = (go_weekday + 6) % 7`, 0 = Monday.
- Card order is one KV key, `zone_order`, filtered to existing zones with
  missing ones appended, so a stale value always renders the current set.
  Drag is pointer events (touch + mouse), not HTML5 DnD; `touch-action:none`
  on the grip.
- The 5 s poll skips the rebuild when a `signature()` of the payload is
  unchanged. `remaining_s` is excluded, since the countdown is recomputed
  locally. The mechanism behind the Android "tap before every scroll" report
  was inferred, not observed.
- valve_watch is stateless apart from a per-zone `alerted:<zone>` flag: the
  baseline and the demand duration are read from history, so a restart
  mid-episode cannot mis-capture the baseline. `ha.get_history`'s `since` is a
  time value, rendered as a lexical prefix of any same-second `changed_at`.
- `thermostat.lua`'s `apply_zone` still records `written` in every mode (the
  round 5 A2 bug). Left: not live, and only an armed overshoot hold can latch
  there, since its frost is written by `heating_windows.lua`.

## Test gotchas
- The schedule legend is CSS `text-transform:uppercase`, so `chromedp.Text`
  returns upper case; read `textContent` via Evaluate.
- `Poll(editorAnimationsDone)` before clicking in the schedule editor: the open
  animation clips the buttons and clicks miss.
- The language picker test fires its change from `setTimeout(..., 0)` so the
  Evaluate returns before `location.assign` destroys the JS context.
