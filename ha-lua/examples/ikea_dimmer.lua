-- ikea_dimmer.lua
--
-- An IKEA E1743 / RODRET two-button dimmer driving one light, with the
-- hold-to-dim ramp run here in the daemon instead of on the device.
--
-- The dimmer sends "on" / "off" for a click, and for a hold the pair
-- "brightness_move_up" / "brightness_move_down" at press plus
-- "brightness_stop" at release. It never sends a level — it expects whoever
-- is listening to keep stepping the light until the release arrives. That is
-- all start_ramp/ramp_tick do: a chain of ha.after steps, each nudging
-- brightness by STEP and scheduling the next, cancelled by bumping a
-- generation counter that the pending step checks. (The daemon has no
-- timer-cancel API, so a stale step must disarm itself.)
--
-- HOW THE PRESSES GET HERE. Zigbee2MQTT can surface a button three ways, and
-- which ones you have depends on its config — so this script accepts any of
-- them:
--
--   1. As an **MQTT device trigger** (`device_automation` discovery). This is
--      the default in Zigbee2MQTT 2.x and it produces NO entity at all: the
--      press exists only as an automation trigger inside Home Assistant, so
--      no script can see it directly. Bridge it with a four-line HA
--      automation that re-fires the press onto the event bus:
--
--        triggers:
--          - trigger: device
--            domain: mqtt
--            device_id: <your dimmer's device id>
--            type: action
--            subtype: on              # one trigger per subtype you use:
--                                     # on, off, brightness_move_up,
--                                     # brightness_move_down, brightness_stop
--        actions:
--          - event: ha_lua_command
--            event_data:
--              script: ikea_dimmer    # this script's id (its filename)
--              action: "{{ trigger.payload }}"
--
--      ha.on_command below picks those up. This path is the reliable one:
--      every press fires, including two identical presses in a row.
--
--   2. As `event.<name>_action` — state is a timestamp, the action sits in
--      the event_type attribute.
--
--   3. As the legacy `sensor.<name>_action` — state IS the action. This one
--      is lossy: Home Assistant does not fire state_changed when neither the
--      state nor the attributes changed, so a second identical press in a
--      row is simply never delivered.
--
--   If an action entity exists the script watches it (2 before 3) and logs
--   which one at load; otherwise it relies on path 1 alone.
--
-- The other hard-won detail is that the light's REPORTED brightness lags what
-- we commanded by the Zigbee round trip, so seeding a new ramp from it right
-- after the previous one ended would jump the level backwards. A ramp seeds
-- from the level we last commanded while that is still fresh, and only then
-- from the reported state. Same lesson as mirrored_switches.lua.
--
-- Edit DIMMER and LIGHT for your own devices. RAMP_FULL_SECS is the knob for
-- how fast a hold crosses the whole range.

-- A hold is a race against the user's finger: the default 100 ms batch window
-- would both delay the ramp and, for a short tap, collapse the move and its
-- stop into one event — losing the hold entirely.
ha.immediate_events()

local DIMMER = "ikea_dimmer_1" -- Zigbee2MQTT friendly name, not an entity id
local LIGHT = "light.konyha_konyha_led"

local ON_BRIGHTNESS = 255 -- what a click on the on button sets; nil = the bulb's own last level
local RAMP_STEP_SPEC = "250ms"
local RAMP_STEP_SECS = 0.25
local RAMP_FULL_SECS = 4 -- seconds a hold takes to cross the whole range
local MIN_BRIGHTNESS = 3 -- a hold down dims to the bottom, it never switches off
local MAX_BRIGHTNESS = 255
local COMMAND_FRESH_SECS = 5

local STEP = math.ceil((MAX_BRIGHTNESS - MIN_BRIGHTNESS) * RAMP_STEP_SECS / RAMP_FULL_SECS)

local ramp = { generation = 0, level = nil }
local commanded = { level = nil, at = 0 }

-- Everything this script does is one press turning into a burst of service
-- calls a second later; when it misbehaves the only useful question is "what
-- did the dimmer send and what did we compute from it". Run the add-on at
-- log_level: debug to get that trace.
local function trace(msg)
  ha.log("debug", DIMMER .. ": " .. msg)
end

local function set_brightness(level)
  commanded.level, commanded.at = level, os.time()
  ha.call_service("light", "turn_on", {
    entity_id = LIGHT,
    brightness = level,
    -- Glide over the whole step so the ramp reads as one movement instead of
    -- a staircase.
    transition = RAMP_STEP_SECS,
  }, { wait = false })
end

-- Where a ramp starts from, or nil when the light is off.
local function current_level()
  local light = ha.get_state(LIGHT)
  if not light then
    trace("ramp seed: " .. LIGHT .. " is unknown to the daemon")
    return nil
  end
  if light.state ~= "on" then
    trace("ramp seed: light is " .. tostring(light.state))
    return nil
  end
  local age = os.time() - commanded.at
  if commanded.level and age <= COMMAND_FRESH_SECS then
    trace(string.format("ramp seed: %d (commanded %ds ago, report may lag)",
      commanded.level, age))
    return commanded.level
  end
  local brightness = light.attributes and light.attributes.brightness
  if type(brightness) ~= "number" then
    trace("ramp seed: on, but no brightness reported; assuming " .. MAX_BRIGHTNESS)
    return MAX_BRIGHTNESS
  end
  trace(string.format("ramp seed: %d (reported)", brightness))
  return brightness
end

local function clamp(level)
  if level < MIN_BRIGHTNESS then
    return MIN_BRIGHTNESS
  elseif level > MAX_BRIGHTNESS then
    return MAX_BRIGHTNESS
  end
  return level
end

local ramp_tick
ramp_tick = function(generation, direction)
  if generation ~= ramp.generation then
    trace(string.format("step %d cancelled (generation is %d)", generation, ramp.generation))
    return -- released, or reversed, before this step got its turn
  end
  local next_level = clamp(ramp.level + direction * STEP)
  if next_level == ramp.level then
    trace(string.format("ramp %d pinned at %d, stopping", generation, next_level))
    return -- at the end of the range; stop rather than resend the same level
  end
  trace(string.format("ramp %d step %d -> %d", generation, ramp.level, next_level))
  ramp.level = next_level
  set_brightness(next_level)
  ha.after(RAMP_STEP_SPEC, function() ramp_tick(generation, direction) end)
end

local function start_ramp(direction)
  ramp.generation = ramp.generation + 1
  trace(string.format("ramp %d start, direction %+d, step %d every %s",
    ramp.generation, direction, STEP, RAMP_STEP_SPEC))
  local level = current_level()
  if not level then
    if direction < 0 then
      trace("nothing to dim, light is off")
      return
    end
    -- Holding up on a dark light lights it at the bottom and ramps from
    -- there, which is what the dimmer would do driving the bulb directly.
    level = MIN_BRIGHTNESS
    ramp.level = level
    set_brightness(level)
  else
    ramp.level = level
  end
  ramp_tick(ramp.generation, direction) -- first step now: a hold must not feel laggy
end

local function stop_ramp()
  if ramp.level then
    trace(string.format("ramp %d released at %d", ramp.generation, ramp.level))
  end
  ramp.generation = ramp.generation + 1
end

local ACTIONS = {
  on = function()
    if ON_BRIGHTNESS then
      set_brightness(ON_BRIGHTNESS)
    else
      commanded.level = nil
      ha.call_service("light", "turn_on", { entity_id = LIGHT }, { wait = false })
    end
  end,
  off = function()
    commanded.level = nil
    ha.call_service("light", "turn_off", { entity_id = LIGHT }, { wait = false })
  end,
  brightness_move_up = function() start_ramp(1) end,
  brightness_move_down = function() start_ramp(-1) end,
  brightness_stop = stop_ramp,
}

local function dispatch(name, source)
  local action = ACTIONS[name]
  if not action then
    trace(string.format("ignoring unknown action %q from %s", tostring(name), source))
    return
  end
  trace(string.format("action %s from %s", name, source))
  action()
end

-- Path 1: the HA automation bridging the MQTT device trigger. on_command keeps
-- only the events addressed to this script (event_data.script == script id).
ha.on_command(function(action)
  dispatch(action, "ha_lua_command")
end)

-- Diagnostic only. on_command drops a command addressed to another script
-- silently, which from the log looks exactly like no command arriving at all —
-- and those two have completely different causes (a wrong `script:` in the
-- automation vs. the automation not firing, or the daemon not subscribed).
-- This says which one you are looking at.
ha.on_event("ha_lua_command", function(data)
  trace(string.format("ha_lua_command seen: script=%s action=%s (mine: %s)",
    tostring(data and data.script), tostring(data and data.action), ha.script_id))
end)

-- Paths 2 and 3: an action entity, if this Zigbee2MQTT publishes one. The
-- event platform carries the action in an attribute (its state is only a
-- timestamp); the legacy sensor's state is the action itself.
local function action_of(new_state)
  local attributes = new_state.attributes
  if attributes and type(attributes.event_type) == "string" then
    return attributes.event_type
  end
  return new_state.state
end

local function action_entity()
  local event_id, sensor_id
  for _, entity_id in ipairs(ha.get_entity_ids("*" .. DIMMER .. "*_action")) do
    if entity_id:match("^event%.") then
      event_id = entity_id
    elseif entity_id:match("^sensor%.") then
      sensor_id = entity_id
    end
  end
  return event_id or sensor_id
end

local ACTION_ENTITY = action_entity()
if ACTION_ENTITY then
  ha.log("info", DIMMER .. ": watching " .. ACTION_ENTITY .. " and ha_lua_command for " .. LIGHT)
  ha.on_state_change(ACTION_ENTITY, function(data)
    if data.new_state then
      dispatch(action_of(data.new_state), data.entity_id)
    end
  end)
else
  -- Not a warning: Zigbee2MQTT 2.x publishes the button as an MQTT device
  -- trigger only, and then the bridging automation is the whole input.
  ha.log("info", DIMMER .. ": no *_action entity, driving " .. LIGHT ..
    " from ha_lua_command events only (script id " .. ha.script_id .. ")")
end
