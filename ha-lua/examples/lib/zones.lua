-- lib/zones.lua
--
-- Shared zone definitions for thermostat.lua, heating_windows.lua and
-- valve_watch.lua. All scripts MUST agree on the zone keys: a key is the
-- <zone> in the published desired setpoint that hands control off between the
-- thermostat and the window script. Keeping the table (and the key builder)
-- here is what stops the scripts from drifting.
--
-- REPLACE the entity ids below with your own Home Assistant entities. The
-- ids here are placeholders and will not match anything in your setup. Add or
-- remove zones freely; only the keys have to stay consistent across scripts.

local M = {}

-- Setpoint (°C) the window script holds while a window in a zone is open.
M.frost_temp = 15

-- Seed value (°C) for a zone's comfort/boost temperature, used the first time
-- before the user touches that zone's stepper in the UI.
M.default_comfort = 23

-- One entry per zone. `windows` is a list so a zone can have several sensors.
-- `radiator` is the temperature sensor strapped to that zone's radiator; only
-- valve_watch.lua reads it (to spot a stuck/dead valve), the thermostat and
-- window scripts ignore it.
M.zones = {
  livingroom = { climate = "climate.living_room", windows = { "binary_sensor.living_room_window" }, radiator = "sensor.living_room_radiator_temp" },
  bedroom    = { climate = "climate.bedroom",     windows = { "binary_sensor.bedroom_window" },     radiator = "sensor.bedroom_radiator_temp" },
  kitchen    = { climate = "climate.kitchen",     windows = { "binary_sensor.kitchen_window" },     radiator = "sensor.kitchen_radiator_temp" },
}

-- The global key both scripts use to hand off the controller's desired
-- setpoint. The controller publishes it every tick; the window script reads it
-- to know what to restore when a window closes.
function M.desired_key(zone)
  return "thermostat:desired:" .. zone
end

return M
