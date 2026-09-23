-- heating_windows.lua
--
-- Owns the window dimension of each zone's setpoint: while a window in a zone is
-- open the heating drops to the frost guard, and closing the last one restores
-- it. It cooperates with thermostat.lua rather than fighting it (see
-- thermostat-ui-spec.md §4.2):
--
--   * on close it restores what the controller is commanding NOW — the value it
--     publishes to global:thermostat:written:<zone> — so a schedule transition
--     or override during the airing is honoured. Deliberately the *written*
--     value and not the requested one: restoring the request would wipe an
--     active overshoot correction for the rest of the warmup
--     (overshoot-spec.md §7).
--   * the controller never writes the setpoint while a window is open, so the
--     two can never write conflicting values.
--
-- Only acts while the climate entity is in hvac mode "heat".

local zones = require "zones"
local control = require "control"

local FROST = zones.frost_temp
local zone_defs = zones.zones

-- Which zone a firing sensor belongs to; a zone may list several sensors.
local by_window = {}
for zone, conf in pairs(zone_defs) do
  for _, window in ipairs(conf.windows) do
    by_window[window] = zone
  end
end

-- The climate entity's state IS its hvac mode: "heat", not "off".
local function is_heating(zone)
  local state = ha.get_state(zone_defs[zone].climate)
  return state ~= nil and state.state == "heat"
end

-- The setpoint belongs to the zone, not to the sensor that fired: restoring when
-- one of two windows closes would heat the room with the other still open. An
-- unseeded sensor counts as closed.
local function any_window_open(zone)
  local states = {}
  for _, window in ipairs(zone_defs[zone].windows) do
    local state = ha.get_state(window)
    states[#states + 1] = state and state.state or "off"
  end
  return control.window_open(states)
end

local function set_temp(zone, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id = zone_defs[zone].climate,
    temperature = temp,
  })
end

-- Door/window binary_sensor convention: "on" = open, "off" = closed.
for window in pairs(by_window) do
  ha.on_state_change(window, function(data)
    local new_state = data.new_state and data.new_state.state
    local zone = by_window[data.entity_id]
    if not is_heating(zone) then return end

    if new_state == "on" then
      set_temp(zone, FROST)
    elseif new_state == "off" and not any_window_open(zone) then
      -- Whatever the controller is commanding now, never the pre-open
      -- setpoint: a schedule transition or override during the airing wins.
      local commanded = global.get(zones.written_key(zone))
      if type(commanded) == "number" then set_temp(zone, commanded) end
    end
  end)
end

ha.on_exception(ha.exceptions.log_file("heating_windows-errors.log"))
