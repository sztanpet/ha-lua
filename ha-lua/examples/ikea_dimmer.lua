-- ikea_dimmer.lua
--
-- An IKEA E1743 / RODRET two-button dimmer driving one light, straight off
-- the MQTT broker — no Home Assistant automation in the path.
--
-- The dimmer sends "on" / "off" for a click, and for a hold the pair
-- "brightness_move_up" / "brightness_move_down" at press plus
-- "brightness_stop" at release. It never sends a level: it expects whoever is
-- listening to keep stepping the light until the release arrives. That is all
-- start_ramp/ramp_tick do — a chain of ha.after steps, each nudging
-- brightness by STEP and scheduling the next, cancelled by bumping a
-- generation counter that the pending step checks. (The daemon has no
-- timer-cancel API, so a stale step must disarm itself.)
--
-- WHY MQTT AND NOT AN ENTITY. Zigbee2MQTT 2.x publishes a button as an MQTT
-- **device trigger**: it produces no entity, and Home Assistant consumes the
-- press inside its automation engine, so it never reaches the event bus and
-- nothing on the HA WebSocket API can see it. The broker is the only place
-- that press exists. Subscribing here also drops two hops (MQTT → HA →
-- automation → bus → WS) from the most latency-sensitive thing in the house.
--
-- Zigbee2MQTT publishes each press twice: as a bare word on
-- "<base>/<device>/action" and inside the JSON state object on
-- "<base>/<device>". This subscribes to the action topic only — one message
-- per press, no dedup needed. Note the topic carries the friendly name
-- VERBATIM, spaces and all ("zigbee2mqtt/ikea dimmer 1/action"), which is not
-- the underscored entity id HA would show you.
--
-- The light is driven through HA rather than MQTT because it is not a
-- Zigbee2MQTT device. If yours is, publishing {"brightness_move": ±rate} to
-- "<base>/<light>/set" (and 0 to stop) is better still: the bulb ramps
-- itself, and the step chain below disappears entirely.
--
-- The other hard-won detail is that the light's REPORTED brightness lags what
-- we commanded by the Zigbee round trip, so seeding a new ramp from it right
-- after the previous one ended would jump the level backwards. A ramp seeds
-- from the level we last commanded while that is still fresh, and only then
-- from the reported state. Same lesson as mirrored_switches.lua.

ha.immediate_events()

-- The Zigbee2MQTT friendly name, exactly as it appears in the topic —
-- spaces included. Check yours with: mosquitto_sub -t 'zigbee2mqtt/#' -v
local DIMMER = "ikea dimmer 1"
local ACTION_TOPIC = "zigbee2mqtt/" .. DIMMER .. "/action"
local LIGHT = "light.konyha_konyha_led"

local ON_BRIGHTNESS = 255 -- what a click on the on button sets; nil = the bulb's own last level
local RAMP_STEP_SPEC = "150ms"
local RAMP_STEP_SECS = 0.15
local RAMP_FULL_SECS = 8 -- seconds a hold takes to cross the whole range
local MIN_BRIGHTNESS = 3 -- a hold down dims to the bottom, it never switches off
local MAX_BRIGHTNESS = 255
local COMMAND_FRESH_SECS = 5

-- The ramp is geometric, not linear, because the eye is: brightness 20 -> 36
-- is an obvious jump while 200 -> 216 is invisible, yet both are +16. Each
-- step multiplies (or divides) the level by STEP_FACTOR, so every step looks
-- the same size and the whole range still takes RAMP_FULL_SECS. A linear step
-- is exactly what makes a ramp look like a staircase at the dim end.
--
-- RAMP_FULL_SECS is the only knob worth turning: raising it slows the ramp AND
-- shrinks each step, because the step count grows to fill the time.
local STEP_FACTOR = (MAX_BRIGHTNESS / MIN_BRIGHTNESS) ^ (RAMP_STEP_SECS / RAMP_FULL_SECS)

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

-- scale moves one geometric step. The +/-1 floor matters at the bottom of the
-- range, where a multiplicative step rounds back to the level it started from
-- and the ramp would stall instead of reaching the minimum.
local function scale(level, direction)
  if direction > 0 then
    return math.max(math.floor(level * STEP_FACTOR + 0.5), level + 1)
  end
  return math.min(math.floor(level / STEP_FACTOR + 0.5), level - 1)
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
  local next_level = clamp(scale(ramp.level, direction))
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

mqtt.subscribe(ACTION_TOPIC, function(_, action)
  dispatch(action, ACTION_TOPIC)
end)

ha.log("info", DIMMER .. ": watching " .. ACTION_TOPIC .. " for " .. LIGHT)

-- Whether the light honours `transition` decides how smooth a ramp can be:
-- with it, each step glides into the next; without it, every step is an
-- instant jump and the only cure is smaller steps. HA's TRANSITION feature
-- bit (32) is what says which, and it is worth one line at load rather than
-- a guess.
local light = ha.get_state(LIGHT)
if not light then
  ha.log("warn", LIGHT .. " is unknown to the daemon — is that the right entity id?")
else
  local features = tonumber(light.attributes and light.attributes.supported_features) or 0
  local supports_transition = features % 64 >= 32
  ha.log("info", string.format(
    "%s: supported_features=%d transition=%s color_modes=%s brightness=%s",
    LIGHT, features, tostring(supports_transition),
    table.concat(light.attributes.supported_color_modes or {}, ","),
    tostring(light.attributes.brightness)))
end
