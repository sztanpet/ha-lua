-- valve_watch.lua
--
-- Catches a dead radiator valve. The thermostatic valves in each zone fail
-- roughly every two years: they seize shut, so the thermostat keeps calling for
-- heat but no hot water ever reaches the radiator and the room never warms up.
-- The tell-tale is physical: while a zone is calling for heat a *healthy*
-- radiator gets hot within a couple of minutes; a dead valve leaves it sitting
-- at room temperature. This script watches the radiator temperature sensors and
-- sends one notification per heating episode when a zone has been calling for
-- heat for a while but its radiator never warmed up.
--
-- Zone definitions (climate + radiator sensor) live in lib/zones.lua. Edit the
-- knobs below and the NOTIFY_TARGET to match your setup.

local zones = require "zones"

local zone_defs = zones.zones

-- Where to send the alert. A HA notify service, written "<domain>.<service>"
-- (e.g. "notify.pixel_9a" → notify.pixel_9a on your phone).
local NOTIFY_TARGET = "notify.pixel_9a"

-- How long a zone must continuously call for heat before we judge the radiator.
-- A healthy valve warms the radiator well within this window; the slack is to
-- ride out the few minutes it takes hot water to actually reach the metal.
local WARMUP = 15 * time.minute

-- Minimum rise (°C) we expect at the radiator over the warmup window. A seized
-- valve produces a flat line; a healthy one climbs far more than this.
local MIN_RISE = 3.0

-- If the radiator is already this many °C above room temperature, the valve is
-- plainly working (it is hot, just holding) — never alert, even with no rise.
-- This is what stops a false alarm when heat was already demanded before the
-- watch window started and the radiator was hot from the off.
local HOT_MARGIN = 8.0

-- Fallback heat-demand threshold (°C) for climate entities that do not report
-- an hvac_action attribute: current temperature must be this far below target.
local DEMAND_HYSTERESIS = 0.1

-- Human-readable zone names for the notification text.
local zone_labels = {
  bedroom    = "Bedroom",
  livingroom = "Living room",
  childrens  = "Children's room",
}

local function label(zone)
  return zone_labels[zone] or zone
end

local function watch_key(zone)
  return "watch:" .. zone
end

local function parse_time(text)
  if type(text) ~= "string" then return nil end
  return time.parse(time.RFC3339, text) -- nil on parse failure
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

-- radiator_temp returns the zone's radiator sensor reading as a number, or nil
-- when the sensor is missing/unavailable/unknown (its state is not numeric).
local function radiator_temp(zone)
  local state = ha.get_state(zone_defs[zone].radiator)
  if state == nil then return nil end
  return tonumber(state.state)
end

-- calling_for_heat reports whether the zone's thermostat currently wants heat,
-- i.e. the valve *should* be open. Prefer the entity's own hvac_action
-- ("heating" vs "idle"); fall back to comparing current temperature to target
-- for climate entities that do not publish hvac_action.
local function calling_for_heat(zone)
  local state = ha.get_state(zone_defs[zone].climate)
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

local function notify(zone, current_rad, base_rad, elapsed_s)
  local domain, service = NOTIFY_TARGET:match("^(.-)%.(.+)$")
  if domain == nil then
    ha.log("error", "NOTIFY_TARGET must be '<domain>.<service>': " .. NOTIFY_TARGET)
    return
  end
  local minutes = math.floor(elapsed_s / 60)
  local message = string.format(
    "%s radiator is still %.1f°C (was %.1f°C) after %d min of calling for heat. "
      .. "The valve may be stuck/dead.",
    label(zone), current_rad, base_rad, minutes)
  ha.call_service(domain, service, {
    title = "Heating valve fault?",
    message = message,
  })
  ha.log("warn", "valve_watch: " .. message)
end

-- check_zone advances the per-zone state machine for one tick. The watch record
-- ({ since, base, notified }) is created when heat demand starts, evaluated once
-- the warmup window has elapsed, and torn down as soon as demand clears so the
-- next heating episode re-arms a fresh alert.
local function check_zone(zone, now)
  local current_rad = radiator_temp(zone)

  -- No demand, or no usable radiator reading: nothing to judge. Drop any watch
  -- so the next genuine episode starts clean.
  if not calling_for_heat(zone) or current_rad == nil then
    store.delete(watch_key(zone))
    return
  end

  local watch = store.get(watch_key(zone))
  if type(watch) ~= "table" or type(watch.since) ~= "string" then
    -- Demand just started: record the baseline radiator temperature.
    store.set(watch_key(zone), { since = now:format(time.RFC3339), base = current_rad, notified = false })
    return
  end

  if watch.notified then return end -- already alerted this episode

  local since = parse_time(watch.since)
  if since == nil then
    store.delete(watch_key(zone))
    return
  end
  if now:sub(since) < WARMUP then return end -- give it time to warm up

  -- The valve is clearly working if the radiator is already hot relative to the
  -- room, regardless of how much it rose inside the window.
  local room = room_temp(zone)
  if room ~= nil and (current_rad - room) >= HOT_MARGIN then return end

  if (current_rad - (watch.base or current_rad)) < MIN_RISE then
    notify(zone, current_rad, watch.base or current_rad, now:sub(since))
    watch.notified = true
    store.set(watch_key(zone), watch)
  end
end

ha.every("1m", function()
  local now = time.now()
  for zone in pairs(zone_defs) do
    check_zone(zone, now)
  end
end)

ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/valve_watch-errors.log"))
