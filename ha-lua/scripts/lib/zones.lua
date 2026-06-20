-- lib/zones.lua
--
-- Shared zone definitions for thermostat.lua and heating_windows.lua. Both
-- scripts MUST agree on the zone keys: a key is the <zone> in the published
-- desired setpoint that hands control off between the two scripts. Keeping the
-- table (and the key builder) here is what stops the two scripts from drifting.
--
-- Edit the entity ids below to match your Home Assistant setup.

local M = {}

-- Setpoint (°C) the window script holds while a window in a zone is open.
M.frost_temp = 15

-- Seed value (°C) for a zone's comfort/boost temperature, used the first time
-- before the user touches that zone's stepper in the UI.
M.default_comfort = 21

-- One entry per zone. `windows` is a list so a zone can have several sensors.
M.zones = {
  bedroom    = { climate = "climate.bedroom",        windows = { "binary_sensor.bedroom_window" } },
  livingroom = { climate = "climate.livingroom",     windows = { "binary_sensor.livingroom_window" } },
  childrens  = { climate = "climate.childrens_room",  windows = { "binary_sensor.childrens_room_window" } },
}

-- The global key both scripts use to hand off the controller's desired
-- setpoint. The controller publishes it every tick; the window script reads it
-- to know what to restore when a window closes.
function M.desired_key(zone)
  return "thermostat:desired:" .. zone
end

return M
