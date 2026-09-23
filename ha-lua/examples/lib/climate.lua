-- lib/climate.lua
--
-- The reads thermostat.lua and enhanced_climate.lua both make of a climate
-- entity and of the clock. Identical in both, and subtle enough to be worth
-- having once: the weekday conversion off Go's Sunday-first ordering, and the
-- bounds fallback that decides what an unseeded entity is allowed to be set to.
-- The decisions taken on these values live in lib/control.lua.

local M = {}

-- The time, plus the schedule's weekday (0=Mon..6=Sun) and minute-of-day.
function M.now_parts()
  local now = time.now()
  local dow = (now:weekday() + 6) % 7
  return now, dow, now:hour() * 60 + now:minute()
end

-- An RFC3339 instant, or nil when it is absent or malformed.
function M.parse_time(text)
  if type(text) ~= "string" then return nil end
  return time.parse(time.RFC3339, text)
end

-- The entity's hvac mode ("heat"/"off"/...), or nil until it seeds.
function M.mode(entity)
  local state = ha.get_state(entity)
  if state == nil then return nil end
  return state.state
end

local function attribute(entity, name)
  local state = ha.get_state(entity)
  if state and state.attributes then return state.attributes[name] end
  return nil
end

-- The setpoint currently on the device.
function M.target(entity)
  return attribute(entity, "temperature")
end

-- The room temperature the device reports.
function M.current_temp(entity)
  return attribute(entity, "current_temperature")
end

-- The device's accepted setpoint range. HA silently drops a set_temperature
-- outside min_temp/max_temp, so honouring the device's own limits is what keeps
-- a schedule or override from becoming a no-op nobody can see. The 5..35
-- fallback covers the window before the entity seeds.
function M.bounds(entity)
  local lo, hi = 5, 35
  local min_temp, max_temp = attribute(entity, "min_temp"), attribute(entity, "max_temp")
  if type(min_temp) == "number" then lo = min_temp end
  if type(max_temp) == "number" then hi = max_temp end
  return lo, hi
end

return M
