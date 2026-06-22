-- heating_windows.lua
--
-- Owns the *window* dimension of each zone's setpoint: while a window in a zone
-- is open the heating drops to the frost guard (15°C); when it closes again the
-- setpoint is restored. It cooperates with thermostat.lua instead of fighting
-- it (see thermostat-ui-spec.md §4.2):
--
--   * on close it restores whatever the controller currently *wants* — the
--     value the controller publishes to global:thermostat:desired:<zone> — not
--     a stale pre-open setpoint. So a schedule transition or a boost that
--     happened while the window was open is honoured on close.
--   * the controller, in turn, never writes the setpoint while a window is
--     open, so the two can never write conflicting values.
--
-- Only acts while the climate entity is actually heating (hvac mode "heat");
-- when it is "off" the zone is left untouched.

local zones = require "zones"

local FROST = zones.frost_temp
local zone_defs = zones.zones

-- Reverse lookup so a window callback can find its zone key from the sensor
-- that fired. A zone may list several window sensors.
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

local function set_temp(zone, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id = zone_defs[zone].climate,
    temperature = temp,
  })
end

-- Door/window binary_sensor convention: "on" = open, "off" = closed.
for window in pairs(by_window) do
  ha.on_state_change(window, function(data)
    local new_state = data.new_state
    if new_state == nil then return end
    local zone = by_window[data.entity_id]
    if not is_heating(zone) then return end -- mode must be "heat", not "off"

    if new_state.state == "on" then
      -- Opened: drop to the frost guard. No need to save anything — the
      -- controller keeps publishing what it wants in global.
      set_temp(zone, FROST)
    elseif new_state.state == "off" then
      -- Closed: restore whatever the controller currently wants. This is the
      -- live schedule/boost value, never the stale pre-open setpoint.
      local desired = global.get(zones.desired_key(zone))
      if type(desired) == "number" then set_temp(zone, desired) end
    end
  end)
end

ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/heating_windows-errors.log"))
