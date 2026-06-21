-- lib/zones.lua
--
-- Shared zone definitions for thermostat.lua, heating_windows.lua and
-- valve_watch.lua. All scripts MUST agree on the zone keys: a key is the
-- <zone> in the published desired setpoint that hands control off between the
-- thermostat and the window script. Keeping the table (and the key builder)
-- here is what stops the scripts from drifting.
--
-- Edit the entity ids below to match your Home Assistant setup.

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
  bedroom    = { climate = "climate.konyha_halo_futes",        windows = { "binary_sensor.ikea_door_5_contact" }, radiator = "sensor.konyha_halo_radiator_temp" },
  livingroom = { climate = "climate.konyha_nappali_futes",     windows = { "binary_sensor.ikea_door_2_contact" }, radiator = "sensor.konyha_nappali_radiator_temp" },
  childrens  = { climate = "climate.konyha_gyerekszoba_futes",  windows = { "binary_sensor.ikea_door_3_contact" }, radiator = "sensor.konyha_gyerekszoba_radiator_temp" },
}

-- The global key both scripts use to hand off the controller's desired
-- setpoint. The controller publishes it every tick; the window script reads it
-- to know what to restore when a window closes.
function M.desired_key(zone)
  return "thermostat:desired:" .. zone
end

return M
