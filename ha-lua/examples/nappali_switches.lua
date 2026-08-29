-- nappali_switches.lua
--
-- Two Aqara WXKG03LM single-rocker buttons driving the two living-room
-- lights straight off the MQTT broker — no Home Assistant automation in the
-- path.
--
--   switch1 single  -> toggle light.zbminir2_nappaliablak
--   switch2 single  -> toggle light.zbminir2_nappalicsillar
--   double or hold  -> both lights, as a group
--
-- WHY MQTT AND NOT AN ENTITY. Zigbee2MQTT 2.x publishes a button as an MQTT
-- **device trigger**: it creates no entity, and Home Assistant consumes the
-- press inside its automation engine, so it never reaches the event bus and
-- nothing on the HA WebSocket API can see it. The broker is the only place
-- the press exists — which is why this subscribes to it directly rather than
-- watching an entity.
--
-- "Both", for a double or a hold, is a GROUP toggle rather than two
-- independent toggles: if either light is on both go off, otherwise both come
-- on. Toggling each one independently would leave a half-lit room half-lit —
-- the gesture meant to kill the whole room would only swap which lamp is on.
--
-- The device sends exactly one word per gesture ("single", "double", "hold")
-- and never a "single" ahead of a "double", so a click acts immediately
-- instead of waiting out a double-click window. The action topic is not
-- retained either, so reloading this script does not replay the last press.
--
-- Topics carry the Zigbee2MQTT friendly name verbatim, not the underscored
-- entity id. Both of these were confirmed against the live broker
-- (2026-08-29) with:
--
--   mosquitto_sub -h <broker> -t 'zigbee2mqtt/switch+/#' -v

local BUTTONS = {
  { topic = "zigbee2mqtt/switch1/action", light = "light.zbminir2_nappaliablak" },
  { topic = "zigbee2mqtt/switch2/action", light = "light.zbminir2_nappalicsillar" },
}

local ALL_LIGHTS = {}
for _, button in ipairs(BUTTONS) do
  table.insert(ALL_LIGHTS, button.light)
end

-- One press turns into a service call a beat later; when it misbehaves the
-- only useful question is what the button sent and what we made of it. Run
-- the add-on at log_level: debug to get that trace.
local function trace(message)
  ha.log("debug", "nappali: " .. message)
end

-- wait = false: a wall button must not park the event loop for the Zigbee
-- round trip. Failures still reach ha.on_exception.
local function command(service, entity_id)
  ha.call_service("light", service, { entity_id = entity_id }, { wait = false })
end

local function any_light_on()
  for _, light in ipairs(ALL_LIGHTS) do
    local state = ha.get_state(light)
    if state and state.state == "on" then
      return true
    end
  end
  return false
end

local function toggle_all()
  local service = any_light_on() and "turn_off" or "turn_on"
  trace("both lights -> " .. service)
  command(service, ALL_LIGHTS)
end

local ACTIONS = {
  double = toggle_all,
  hold = toggle_all,
}

for _, button in ipairs(BUTTONS) do
  local light = button.light
  mqtt.subscribe(button.topic, function(topic, action)
    if action == "single" then
      trace(topic .. ": single -> toggle " .. light)
      command("toggle", light)
      return
    end
    local handler = ACTIONS[action]
    if not handler then
      trace(string.format("%s: ignoring unknown action %q", topic, tostring(action)))
      return
    end
    trace(topic .. ": " .. action)
    handler()
  end)

  if not ha.get_state(light) then
    ha.log("warn", light .. " is unknown to the daemon — is that the right entity id?")
  end
  ha.log("info", "nappali: watching " .. button.topic .. " for " .. light)
end
