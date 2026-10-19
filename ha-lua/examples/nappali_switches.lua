-- nappali_switches.lua
--
-- Two Aqara WXKG03LM rockers driving lights straight off the MQTT broker:
--
--   switch1 single         -> toggle light.zbminir2_nappaliablak
--   switch1 double or hold -> both living-room lights, as a group
--   switch2 single         -> toggle light.konyha_konyha_led
--   switch2 double or hold -> toggle light.zbminir2_nappalicsillar
--
-- WHY MQTT AND NOT AN ENTITY. Zigbee2MQTT 2.x publishes a button as an MQTT
-- device trigger: it creates no entity, and Home Assistant consumes the press
-- inside its automation engine, so it never reaches the event bus and nothing on
-- the HA WebSocket API can see it. The broker is the only place the press
-- exists.
--
-- A gesture naming several lights is a GROUP toggle: if any of them is on they
-- all go off, otherwise they all come on. Toggling each independently would
-- leave a half-lit room half-lit, swapping which lamp is on instead of clearing
-- the room.
--
-- The device sends one word per gesture and never a "single" ahead of a
-- "double", so a click acts immediately rather than waiting out a double-click
-- window. The action topic is not retained either, so a reload cannot replay
-- the last press.
--
-- Topics carry the Zigbee2MQTT friendly name verbatim, spaces included, not the
-- underscored entity id. Confirm yours against the broker before editing:
--
--   mosquitto_sub -h <broker> -t 'zigbee2mqtt/switch+/#' -v

local ABLAK = "light.zbminir2_nappaliablak"
local CSILLAR = "light.zbminir2_nappalicsillar"
local KONYHA_LED = "light.konyha_konyha_led"

local BUTTONS = {
  {
    topic = "zigbee2mqtt/switch1/action",
    actions = {
      single = { ABLAK },
      double = { ABLAK, CSILLAR },
      hold = { ABLAK, CSILLAR },
    },
  },
  {
    topic = "zigbee2mqtt/switch2/action",
    actions = {
      single = { KONYHA_LED },
      double = { CSILLAR },
      hold = { CSILLAR },
    },
  },
}

-- When a press misbehaves the only useful question is what the button sent and
-- what we made of it. Run the add-on at log_level: debug for that trace.
local function trace(message)
  ha.log("debug", "nappali: " .. message)
end

-- wait = false: a wall button must not park the event loop for the Zigbee
-- round trip.
local function command(service, entity_id)
  ha.call_service("light", service, { entity_id = entity_id }, { wait = false })
end

local function any_light_on(lights)
  for _, light in ipairs(lights) do
    local state = ha.get_state(light)
    if state and state.state == "on" then
      return true
    end
  end
  return false
end

local function switch(lights)
  if #lights == 1 then
    command("toggle", lights[1])
    return
  end
  command(any_light_on(lights) and "turn_off" or "turn_on", lights)
end

local seen = {}
for _, button in ipairs(BUTTONS) do
  local actions = button.actions
  mqtt.subscribe(button.topic, function(topic, action)
    local lights = actions[action]
    if not lights then
      trace(string.format("%s: ignoring unknown action %q", topic, tostring(action)))
      return
    end
    trace(topic .. ": " .. action .. " -> " .. table.concat(lights, ","))
    switch(lights)
  end)

  for _, lights in pairs(actions) do
    for _, light in ipairs(lights) do
      if not seen[light] and not ha.get_state(light) then
        ha.log("warn", light .. " is unknown to the daemon — is that the right entity id?")
      end
      seen[light] = true
    end
  end
  ha.log("info", "nappali: watching " .. button.topic)
end
