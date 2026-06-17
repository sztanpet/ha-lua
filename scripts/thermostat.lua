-- thermostat.lua
--
-- The heating controller. It owns the schedule / boost / manual-override
-- dimension of each zone's setpoint; heating_windows.lua owns the window
-- dimension. The split is clean: this script computes one desired setpoint per
-- zone, publishes it to global so the window script can restore it, and writes
-- it to the climate entity only while no window in the zone is open.
--
-- See thermostat-ui-spec.md for the full design. This file is the controller
-- (the 1-minute tick + desired() engine + manual-override detection); the HTTP
-- API and the single-page UI are added on top of it.

local zones = require "zones"
local schedule = require "schedule"

local Z = zones.zones

-- Per-zone store keys. Schedule/boost/override/comfort all live in this
-- script's KV store; the published desired lives in `global` (shared).
local function sched_key(z) return "schedule:" .. z end
local function boost_key(z) return "boost:" .. z end
local function override_key(z) return "override:" .. z end
local function comfort_key(z) return "comfort:" .. z end

-- now_parts returns the current time userdata plus the schedule's weekday
-- (0=Mon..6=Sun, converted from Go's Sunday-first weekday) and minute-of-day.
local function now_parts()
  local n = time.now()
  local dow = (n:weekday() + 6) % 7
  return n, dow, n:hour() * 60 + n:minute()
end

local function parse_time(s)
  if type(s) ~= "string" then return nil end
  local t = time.parse(time.RFC3339, s)
  return t -- nil on parse failure
end

-- comfort returns the zone's UI-settable boost temperature, seeding the default
-- the first time before the user touches the stepper.
local function comfort(z)
  local v = store.get(comfort_key(z))
  if type(v) == "number" then return v end
  return zones.default_comfort
end

-- mode returns the climate entity's hvac mode ("heat"/"off"/...) or nil if the
-- entity is not yet seeded.
local function mode(z)
  local c = ha.get_state(Z[z].climate)
  if c == nil then return nil end
  return c.state
end

local function current_target(z)
  local c = ha.get_state(Z[z].climate)
  if c and c.attributes then return c.attributes.temperature end
  return nil
end

-- any_window_open reports whether any window in the zone is definitely open.
-- A not-yet-seeded sensor (nil) counts as closed for the write decision.
local function any_window_open(z)
  for _, w in ipairs(Z[z].windows) do
    local s = ha.get_state(w)
    if s ~= nil and s.state == "on" then return true end
  end
  return false
end

-- any_window_unknown reports whether any window sensor has no state yet. Used
-- only to suppress manual-override detection until the sensors have seeded.
local function any_window_unknown(z)
  for _, w in ipairs(Z[z].windows) do
    if ha.get_state(w) == nil then return true end
  end
  return false
end

local function load_schedule(z)
  local s = store.get(sched_key(z))
  if type(s) == "table" and type(s.days) == "table" then return s.days end
  return {}
end

-- active_boost returns the live boost table for the zone, or nil. An expired
-- boost is cleared as a side effect so the zone reverts to schedule.
local function active_boost(z, now)
  local b = store.get(boost_key(z))
  if type(b) ~= "table" or not b.active or type(b.ends_at) ~= "string" then
    return nil
  end
  local ends = parse_time(b.ends_at)
  if ends == nil then
    store.delete(boost_key(z))
    return nil
  end
  if now:before(ends) then return b end
  store.delete(boost_key(z))
  return nil
end

-- active_override returns the live manual-override table, or nil, clearing it
-- once its `expires` instant has passed. (We avoid the field name "until"
-- because it is a Lua keyword.)
local function active_override(z, now)
  local o = store.get(override_key(z))
  if type(o) ~= "table" or type(o.temp) ~= "number" or type(o.expires) ~= "string" then
    return nil
  end
  local exp = parse_time(o.expires)
  if exp == nil then
    store.delete(override_key(z))
    return nil
  end
  if now:before(exp) then return o end
  store.delete(override_key(z))
  return nil
end

-- desired implements §4.1: boost beats override beats schedule. Returns the
-- temperature and its source string ("boost"/"override"/"schedule"), or nil if
-- the zone has no schedule at all.
local function desired(z, now, dow, minute)
  if active_boost(z, now) then return comfort(z), "boost" end
  local o = active_override(z, now)
  if o then return o.temp, "override" end
  local temp = schedule.resolve(load_schedule(z), dow, minute)
  if temp ~= nil then return temp, "schedule" end
  return nil, nil
end

local function set_temp(z, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id = Z[z].climate,
    temperature = temp,
  })
end

-- apply_zone publishes the zone's desired setpoint and writes it to the climate
-- entity when the mode is heat and no window is open. The write is skipped when
-- the value is unchanged so we don't spam set_temperature.
local function apply_zone(z, now, dow, minute)
  local d = desired(z, now, dow, minute)
  if d == nil then return end
  global.set(zones.desired_key(z), d)
  if mode(z) ~= "heat" then return end
  if any_window_open(z) then return end -- window script's territory
  local cur = current_target(z)
  if cur == nil or math.abs(cur - d) > 0.05 then
    set_temp(z, d)
  end
end

-- The single tick that drives everything (§8): recompute, publish and (maybe)
-- write every zone. Boost/override expiry is handled inside desired().
local function tick()
  local now, dow, minute = now_parts()
  for z in pairs(Z) do
    apply_zone(z, now, dow, minute)
  end
end

ha.every("1m", tick)

-- Manual setpoint change detection (§9): the controller is the only thing that
-- writes `desired`, and it always writes exactly `desired`, so a climate target
-- that differs from the published desired is an external change by the user. It
-- becomes an ad-hoc override that holds until the next schedule transition.
for z, conf in pairs(Z) do
  ha.on_state_change(conf.climate, function(data)
    local ns = data.new_state
    if ns == nil or ns.attributes == nil then return end
    if ns.state ~= "heat" then return end
    local target = ns.attributes.temperature
    if type(target) ~= "number" then return end

    local now, dow, minute = now_parts()
    if active_boost(z, now) then return end -- boost wins; ignore dial nudges
    -- Window open or not-yet-seeded: that's the window script's 15°C territory.
    if any_window_open(z) or any_window_unknown(z) then return end

    local pub = global.get(zones.desired_key(z))
    -- Float tolerance: our own write (and the window restore) set target == pub
    -- exactly, but 21 vs 21.0 must not look like a manual change.
    if type(pub) == "number" and math.abs(target - pub) <= 0.1 then return end

    local _, _, mins_to_next = schedule.resolve(load_schedule(z), dow, minute)
    local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
    store.set(override_key(z), {
      temp = target,
      expires = now:add(hold):format(time.RFC3339),
    })
    apply_zone(z, now, dow, minute) -- republish the new desired immediately
  end)
end

-- Publish each zone's desired once at load time so the window script has a
-- value to restore before the first tick fires.
do
  local now, dow, minute = now_parts()
  for z in pairs(Z) do
    local d = desired(z, now, dow, minute)
    if d ~= nil then global.set(zones.desired_key(z), d) end
  end
end

ha.on_exception(ha.exceptions.log_file("/addon_config/thermostat-errors.log"))
