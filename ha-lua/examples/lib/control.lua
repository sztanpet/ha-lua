-- lib/control.lua
--
-- Pure heating-control helpers, free of any ha.* / store.* / time access so they
-- can be unit-tested directly from Go. The callers do the clock and I/O work and
-- pass plain values. These five decisions are identical in thermostat.lua and
-- enhanced_climate.lua, so they live here once rather than as drifting copies.

local M = {}

-- Setpoint priority: a timed override beats a dial-detected manual hold, which
-- beats the schedule. Each argument is that source's candidate temperature or
-- nil. Returns the winner and its source name, or nil, nil.
function M.desired(override, manual, schedule_temp)
  if override ~= nil then return override, "override" end
  if manual ~= nil then return manual, "manual" end
  if schedule_temp ~= nil then return schedule_temp, "schedule" end
  return nil, nil
end

-- Whether a climate target is an external change rather than our own write.
-- `published` must be what the controller last COMMANDED, not what the user
-- asked for: with an overshoot correction active the two differ and the
-- correction would read as a dial nudge. The 0.1° tolerance keeps 21 vs 21.0
-- from looking like a change; a never-published value counts as manual.
function M.is_manual(target, published)
  if type(published) == "number" and math.abs(target - published) <= 0.1 then
    return false
  end
  return true
end

-- Gates set_temperature: heat mode, no bound window open (the window script's
-- territory), and a target that actually differs, so no-op writes are not spammed
-- once a minute. An unseeded current target always writes.
function M.should_write(mode, window_open, current, target)
  if mode ~= "heat" then return false end
  if window_open then return false end
  if current == nil then return true end
  return math.abs(current - target) > 0.05
end

-- Clamps a setpoint to the device's [lo, hi]. HA silently drops a
-- set_temperature outside min_temp/max_temp, so without this a schedule or
-- override becomes a no-op nobody can see.
function M.clamp_bounds(value, lo, hi)
  if value < lo then return lo end
  if value > hi then return hi end
  return value
end

-- Open if ANY sensor reads "on", clear only when ALL are closed. The caller
-- resolves each sensor to its state string, substituting a non-"on" placeholder
-- for an unseeded one.
function M.window_open(states)
  for _, state in ipairs(states) do
    if state == "on" then return true end
  end
  return false
end

return M
