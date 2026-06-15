-- heating_windows.lua
--
-- Per heating zone: remember the user's setpoint, drop the heating to 15°C
-- while a window in that room is open, and restore the saved setpoint once it
-- closes again. Only acts while the climate entity is actually heating
-- (hvac mode "heat"); if it is "off" the room is left untouched.

local FROST = 15 -- setpoint (°C) to hold while a window is open

-- One entry per room. Rename the entities to match your setup.
local rooms = {
  { climate = "climate.bedroom",        window = "binary_sensor.bedroom_window" },
  { climate = "climate.livingroom",     window = "binary_sensor.livingroom_window" },
  { climate = "climate.childrens_room", window = "binary_sensor.childrens_room_window" },
}

-- Reverse lookups so a callback can find its room from the entity that fired.
local by_climate, by_window = {}, {}
for _, r in ipairs(rooms) do
  by_climate[r.climate] = r
  by_window[r.window] = r
end

local function saved_key(room)
  return "setpoint:" .. room.climate
end

-- Door/window binary_sensor convention: "on" = open, "off" = closed.
local function window_open(room)
  local w = ha.get_state(room.window)
  return w ~= nil and w.state == "on"
end

-- "heat", not "off" — the climate entity's state IS its hvac mode.
local function is_heating(room)
  local c = ha.get_state(room.climate)
  return c ~= nil and c.state == "heat"
end

local function current_setpoint(room)
  local c = ha.get_state(room.climate)
  if c and c.attributes then
    return c.attributes.temperature
  end
  return nil
end

local function set_temp(room, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id   = room.climate,
    temperature = temp,
  })
end

-- 1. Always keep the saved setpoint current — but only while the window is
--    closed, so the 15°C override we apply ourselves is never recorded as the
--    "real" value to restore.
for _, r in ipairs(rooms) do
  ha.on_state_change(r.climate, function(data)
    local ns = data.new_state
    if ns == nil or ns.attributes == nil then return end
    local sp = ns.attributes.temperature
    if sp == nil then return end -- climate may report no setpoint when off
    local room = by_climate[data.entity_id]
    if window_open(room) then return end
    store.set(saved_key(room), sp)
  end, { initial = true })
end

-- 2. React to the windows opening and closing.
for _, r in ipairs(rooms) do
  ha.on_state_change(r.window, function(data)
    local ns = data.new_state
    if ns == nil then return end
    local room = by_window[data.entity_id]
    if not is_heating(room) then return end -- mode must be "heat", not "off"

    if ns.state == "on" then
      -- Opened: capture the live setpoint straight from the mirror (robust
      -- even if a climate event was dropped), then drop to the frost guard.
      local sp = current_setpoint(room)
      if sp ~= nil then store.set(saved_key(room), sp) end
      set_temp(room, FROST)
    elseif ns.state == "off" then
      -- Closed: restore whatever we last saved for this room.
      local saved = store.get(saved_key(room))
      if saved ~= nil then set_temp(room, saved) end
    end
  end)
end

ha.on_exception(ha.exceptions.log_file("/addon_config/heating_windows-errors.log"))
