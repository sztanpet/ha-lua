-- lib/zones.lua
--
-- Shared zone definitions for thermostat.lua, heating_windows.lua and
-- valve_watch.lua. All scripts MUST agree on the zone keys: a key is the
-- <zone> in the published setpoint that hands control off between the
-- thermostat and the window script. Keeping the table (and the key builder)
-- here is what stops the scripts from drifting.
--
-- REPLACE the entity ids below with your own Home Assistant entities. The
-- ids here are placeholders and will not match anything in your setup. Add or
-- remove zones freely; only the keys have to stay consistent across scripts.

local M = {}

-- Setpoint (°C) the window script holds while a window in a zone is open.
M.frost_temp = 15

-- Seed value (°C) for a zone's override temperature (the setpoint an override
-- drives the zone to), used the first time before the user touches that zone's
-- stepper in the UI.
M.default_override_temp = 23

-- One entry per zone. `windows` is a list so a zone can have several sensors.
-- `radiator` is the sensor strapped to that zone's radiator, read by
-- valve_watch.lua. `label` is what a notification calls the zone; it defaults
-- to the key.
M.zones = {
  livingroom = { climate = "climate.living_room", windows = { "binary_sensor.living_room_window" }, radiator = "sensor.living_room_radiator_temp", label = "Living room" },
  bedroom    = { climate = "climate.bedroom",     windows = { "binary_sensor.sonoff_door_1_contact" }, radiator = "sensor.bedroom_radiator_temp", label = "Bedroom" },
  kitchen    = { climate = "climate.kitchen",     windows = { "binary_sensor.kitchen_window" },     radiator = "sensor.kitchen_radiator_temp", label = "Kitchen" },
}

-- The global key the controller publishes each zone's setpoint through, every
-- tick: what it commands the device to. The manual-change detector compares
-- the device against it, and the window script restores it on close.
function M.written_key(zone)
  return "thermostat:written:" .. zone
end

return M
