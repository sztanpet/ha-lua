-- lib/control.lua
--
-- Pure heating-control helpers, deliberately free of any ha.* / store.* / time
-- access so they can be unit-tested directly from Go (like lib/schedule.lua).
-- The controller does the clock and I/O work and calls in here with plain
-- values. These four decisions — the desired-setpoint priority pick, the
-- manual-hold predicate, the write gate, the device-bounds clamp, and the
-- window any-open/all-closed reduction — are identical across thermostat.lua
-- and enhanced_climate.lua, so they live here once rather than as drifting
-- copies (the silent out-of-range fix and the manual tolerances are hard-won).

local M = {}

-- desired implements the setpoint priority: a timed UI override beats a manual
-- (dial-detected) hold, which beats the schedule. Each argument is the candidate
-- temperature for that source, or nil when that source is not active. Returns
-- the winning temperature and its source string ("override"/"manual"/
-- "schedule"), or nil, nil when no source has a value.
function M.desired(override, manual, schedule_temp)
  if override ~= nil then return override, "override" end
  if manual ~= nil then return manual, "manual" end
  if schedule_temp ~= nil then return schedule_temp, "schedule" end
  return nil, nil
end

-- is_manual reports whether a climate target reflects an external (user) change
-- rather than our own write. The controller always writes exactly the published
-- desired, so a target within 0.1° of it is our own write (or the window
-- restore) — 21 vs 21.0 must not look like a change. A non-numeric published
-- value (never published yet) counts as a manual change.
function M.is_manual(target, published)
  if type(published) == "number" and math.abs(target - published) <= 0.1 then
    return false
  end
  return true
end

-- should_write gates the set_temperature call: only write when the climate is
-- in heat mode, no bound window is open (that is the window script's territory),
-- and the new target differs from the current one by more than 0.05° (so we do
-- not spam set_temperature with no-op writes). A nil current target (not seeded)
-- always writes.
function M.should_write(mode, window_open, current, target)
  if mode ~= "heat" then return false end
  if window_open then return false end
  if current == nil then return true end
  return math.abs(current - target) > 0.05
end

-- clamp_bounds clamps a setpoint to the device's accepted [lo, hi] range. HA
-- silently drops a set_temperature outside a climate entity's min_temp/max_temp,
-- so clamping keeps a schedule or override from becoming a no-op the user can
-- never see.
function M.clamp_bounds(value, lo, hi)
  if value < lo then return lo end
  if value > hi then return hi end
  return value
end

-- window_open reduces a list of window sensor state strings to one boolean: open
-- if ANY sensor reads "on", clear only when ALL are closed. The caller resolves
-- each bound sensor to its state string (substituting a non-"on" placeholder for
-- a not-yet-seeded sensor, which counts as closed).
function M.window_open(states)
  for _, state in ipairs(states) do
    if state == "on" then return true end
  end
  return false
end

return M
