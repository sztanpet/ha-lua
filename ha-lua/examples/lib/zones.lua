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

-- Seed value (°C) for a zone's override temperature (the setpoint an override
-- drives the zone to), used the first time before the user touches that zone's
-- stepper in the UI.
M.default_override_temp = 23

-- One entry per zone. `windows` is a list so a zone can have several sensors.
-- `radiator` is the temperature sensor strapped to that zone's radiator; only
-- valve_watch.lua reads it (to spot a stuck/dead valve), the thermostat and
-- window scripts ignore it.
M.zones = {
  livingroom = { climate = "climate.living_room", windows = { "binary_sensor.living_room_window" }, radiator = "sensor.living_room_radiator_temp" },
  bedroom    = { climate = "climate.bedroom",     windows = { "binary_sensor.bedroom_window" },     radiator = "sensor.bedroom_radiator_temp" },
  kitchen    = { climate = "climate.kitchen",     windows = { "binary_sensor.kitchen_window" },     radiator = "sensor.kitchen_radiator_temp" },
}

-- The two global keys the scripts hand zone setpoints off through. Both are
-- published every tick by the controller.
--
-- `desired` is what the user asked for — the schedule/override/manual value.
-- Anything displaying intent reads this one.
--
-- `written` is what the controller actually commands the device to, which may
-- be lower than `desired` while the overshoot correction is cutting a warmup
-- early (see overshoot-spec.md §7). Anything comparing against the value on
-- the device — the manual-change detector, the window script's restore — must
-- read this one, or it will read our own correction as the user turning the
-- dial. The two are equal whenever no correction is active.
function M.desired_key(zone)
  return "thermostat:desired:" .. zone
end

function M.written_key(zone)
  return "thermostat:written:" .. zone
end

return M
