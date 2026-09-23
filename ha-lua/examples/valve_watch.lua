-- valve_watch.lua
--
-- Catches a dead radiator valve. A thermostatic valve that seizes shut leaves
-- the thermostat calling for heat while no hot water reaches the radiator, and
-- the room simply never warms up. The tell-tale is physical: a healthy radiator
-- gets hot within minutes of a zone calling for heat, a dead one stays at room
-- temperature. This sends one notification per heating episode when a zone has
-- called for heat for a while and its radiator never warmed.
--
-- The judgement is read from recorded history rather than a baseline in the
-- store: each tick, a zone calling for heat now AND continuously for the warmup
-- window has its radiator compared against its oldest reading in that window.
-- The only stored state is a per-zone "already alerted this episode" flag.
--
-- Zone definitions (climate + radiator sensor) live in lib/zones.lua. Edit the
-- knobs below and NOTIFY_TARGET to match your setup.

local zones = require "zones"

local zone_defs = zones.zones

-- Where to send the alert. A HA notify service, written "<domain>.<service>"
-- (e.g. "notify.pixel_9a" → notify.pixel_9a on your phone).
local NOTIFY_TARGET = "notify.pixel_9a"

-- How long a zone must call for heat before the radiator is judged. The slack
-- is the few minutes it takes hot water to reach the metal.
local WARMUP = 15 * time.minute

-- Minimum rise (°C) expected across the window. A seized valve is a flat line.
local MIN_RISE = 3.0

-- If the radiator is already this many °C above room temperature, the valve is
-- plainly working (it is hot, just holding) — never alert, even with no rise.
local HOT_MARGIN = 8.0

-- Fallback heat-demand threshold (°C) for climate entities that do not report
-- an hvac_action attribute: current temperature must be this far below target.
local DEMAND_HYSTERESIS = 0.1

-- Cap on history rows per query; generous for a window this short.
local HISTORY_LIMIT = 600

local function label(zone)
  return zone_defs[zone].label or zone
end

-- Per-zone flag so a dead valve is reported once per episode, not once a
-- minute. Cleared as soon as demand stops.
local function alerted_key(zone)
  return "alerted:" .. zone
end

-- demand_active reports whether a climate state table (live or a history row)
-- means the valve *should* be open. Prefer the entity's own hvac_action
-- ("heating" vs "idle"); fall back to current-vs-target for entities without it.
local function demand_active(state)
  if state == nil or state.state ~= "heat" then return false end
  local attrs = state.attributes
  if attrs == nil then return false end
  if type(attrs.hvac_action) == "string" then
    return attrs.hvac_action == "heating"
  end
  local current, target = attrs.current_temperature, attrs.temperature
  if type(current) == "number" and type(target) == "number" then
    return current < target - DEMAND_HYSTERESIS
  end
  return false
end

local function calling_for_heat(zone)
  return demand_active(ha.get_state(zone_defs[zone].climate))
end

-- True when every climate history row since `since` shows demand. An empty
-- window (just after startup) counts as not confirmed, so a zone is never
-- judged before there is history to judge it on.
local function demand_continuous(zone, since)
  local rows = ha.get_history(zone_defs[zone].climate, since, HISTORY_LIMIT)
  if #rows == 0 then return false end
  for _, row in ipairs(rows) do
    if not demand_active(row) then return false end
  end
  return true
end

-- room_temp returns the zone's measured room temperature (the climate entity's
-- current_temperature), or nil if unavailable.
local function room_temp(zone)
  local state = ha.get_state(zone_defs[zone].climate)
  if state and state.attributes then
    local current = state.attributes.current_temperature
    if type(current) == "number" then return current end
  end
  return nil
end

-- radiator_temp returns the zone's radiator sensor reading now as a number, or
-- nil when the sensor is missing/unavailable/unknown (state is not numeric).
local function radiator_temp(zone)
  local state = ha.get_state(zone_defs[zone].radiator)
  if state == nil then return nil end
  return tonumber(state.state)
end

-- The radiator temperature at the start of the window: the oldest numeric
-- reading since `since`, or nil when the sensor never changed in it — which
-- callers read as "same as now", i.e. no rise.
local function radiator_baseline(zone, since)
  local rows = ha.get_history(zone_defs[zone].radiator, since, HISTORY_LIMIT)
  for _, row in ipairs(rows) do
    local value = tonumber(row.state)
    if value ~= nil then return value end
  end
  return nil
end

local function notify(zone, current_rad, base_rad)
  local domain, service = NOTIFY_TARGET:match("^(.-)%.(.+)$")
  if domain == nil then
    ha.log("error", "NOTIFY_TARGET must be '<domain>.<service>': " .. NOTIFY_TARGET)
    return
  end
  local minutes = math.floor(WARMUP / 60)
  local message = string.format(
    "%s radiator is still %.1f°C (was %.1f°C %d min ago) while the thermostat "
      .. "calls for heat. The valve may be stuck/dead.",
    label(zone), current_rad, base_rad, minutes)
  ha.call_service(domain, service, {
    title = "Heating valve fault?",
    message = message,
  })
  ha.log("warn", "valve_watch: " .. message)
end

-- Judges one zone for this tick. No demand drops the alert flag, re-arming the
-- next episode.
local function check_zone(zone, now)
  if not calling_for_heat(zone) then
    store.delete(alerted_key(zone))
    return
  end

  if store.get(alerted_key(zone)) then return end

  local since = now:add(-WARMUP)
  if not demand_continuous(zone, since) then return end

  local current_rad = radiator_temp(zone)
  if current_rad == nil then return end

  -- Already hot relative to the room proves the valve works, whatever it
  -- climbed inside the window.
  local room = room_temp(zone)
  if room ~= nil and (current_rad - room) >= HOT_MARGIN then return end

  local base = radiator_baseline(zone, since) or current_rad
  if (current_rad - base) < MIN_RISE then
    notify(zone, current_rad, base)
    store.set(alerted_key(zone), true)
  end
end

ha.every("1m", function()
  local now = time.now()
  for zone in pairs(zone_defs) do
    check_zone(zone, now)
  end
end)

ha.on_exception(ha.exceptions.log_file("valve_watch-errors.log"))
