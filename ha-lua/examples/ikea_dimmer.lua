-- ikea_dimmer.lua
--
-- An IKEA E1743 / RODRET two-button dimmer driving one light, with the
-- hold-to-dim ramp run here in the daemon instead of on the device.
--
-- Zigbee2MQTT reports the buttons as an *action*: "on" / "off" for a click,
-- and for a hold the pair "brightness_move_up" / "brightness_move_down" at
-- press plus "brightness_stop" at release. The device never sends a level —
-- it expects whoever is listening to keep stepping the light until the
-- release arrives. That is all start_ramp/ramp_tick do: a chain of ha.after
-- steps, each nudging brightness by STEP and scheduling the next, cancelled
-- by bumping a generation counter that the pending step checks. (The daemon
-- has no timer-cancel API, so a stale step must disarm itself.)
--
-- Two Zigbee2MQTT traps this works around:
--
--   * Modern Zigbee2MQTT exposes the action twice: `event.<name>_action`
--     (state is a timestamp, the action sits in the event_type attribute)
--     and the legacy `sensor.<name>_action` (state IS the action). The
--     sensor is the unreliable one — Home Assistant drops a state_changed
--     whose state and attributes are both unchanged, so a second identical
--     press in a row (two "on" clicks, two holds the same way) never reaches
--     any handler. Prefer the event entity, fall back to the sensor.
--
--   * The light's REPORTED brightness lags what we commanded by the Zigbee
--     round trip, so seeding a new ramp from it right after the previous one
--     ended would jump the level backwards. A ramp seeds from the level we
--     last commanded while that is still fresh, and only then from the
--     reported state. Same lesson as mirrored_switches.lua.
--
-- Edit DIMMER and LIGHT for your own devices. RAMP_FULL_SECS is the knob for
-- how fast a hold crosses the whole range.

-- A hold is a race against the user's finger: the default 100 ms batch window
-- would both delay the ramp and, for a short tap, collapse the move and its
-- stop into one event — losing the hold entirely.
ha.immediate_events()

local DIMMER = "ikea_dimmer_1" -- Zigbee2MQTT friendly name, not an entity id
local LIGHT = "light.konyha_konyha_led"

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
    -- No brightness: the bulb restores its own last level, as it does when
    -- the dimmer talks to it directly.
    trace("on: restoring the bulb's own last level")
    commanded.level = nil
    ha.call_service("light", "turn_on", { entity_id = LIGHT }, { wait = false })
  end,
  off = function()
    trace("off")
    commanded.level = nil
    ha.call_service("light", "turn_off", { entity_id = LIGHT }, { wait = false })
  end,
  brightness_move_up = function() start_ramp(1) end,
  brightness_move_down = function() start_ramp(-1) end,
  brightness_stop = stop_ramp,
}

local EVENT_ENTITY = "event." .. DIMMER .. "_action"
local SENSOR_ENTITY = "sensor." .. DIMMER .. "_action"

-- The event platform carries the action in an attribute (its state is only a
-- timestamp); the legacy sensor's state is the action itself.
local function action_of(data)
  local new_state = data.new_state
  if not new_state then
    return nil
  end
  local attributes = new_state.attributes
  if attributes and type(attributes.event_type) == "string" then
    return attributes.event_type
  end
  return new_state.state
end

local function handle(data)
  local name = action_of(data)
  trace("action " .. string.format("%q", tostring(name)) .. " from " .. tostring(data.entity_id))
  local action = ACTIONS[name]
  if not action then
    return -- Zigbee2MQTT clears the action back to "" between presses
  end
  action()
end

-- Subscribe to whichever entity this Zigbee2MQTT version publishes. If both
-- exist the event entity wins, or every press would be handled twice.
if ha.get_state(EVENT_ENTITY) then
  trace("watching " .. EVENT_ENTITY .. " for " .. LIGHT)
  ha.on_state_change(EVENT_ENTITY, handle)
elseif ha.get_state(SENSOR_ENTITY) then
  trace("watching " .. SENSOR_ENTITY .. " for " .. LIGHT ..
    " (legacy action sensor: repeated identical presses are invisible to HA)")
  ha.on_state_change(SENSOR_ENTITY, handle)
else
  ha.log("warn", DIMMER .. ": no action entity yet, watching both " ..
    EVENT_ENTITY .. " and " .. SENSOR_ENTITY .. " (reload once it appears)")
  ha.on_state_change(EVENT_ENTITY, handle)
  ha.on_state_change(SENSOR_ENTITY, handle)
end
