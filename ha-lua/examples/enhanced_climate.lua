-- enhanced_climate.lua
--
-- A card-configured heating controller: each enhanced climate is one HA `climate`
-- entity wrapped with scheduling, timed overrides, manual-change detection and
-- optional window cooperation, all provisioned at RUNTIME from a Lovelace card
-- (custom:ha-lua-enhanced-climate-card) rather than by editing this file. See
-- enhanced-climate-spec.md for the design.
--
-- It runs parallel to thermostat.lua, which keeps the static lib/zones.lua model
-- and an Ingress editor; both share lib/control.lua and lib/schedule.lua and
-- differ only in wiring. Here the definitions live in the card config, mirrored
-- into the daemon over the ha_lua_command event and surfaced back as companion
-- sensors.
--
-- To use it: copy this file and lib/ into /config/ha-lua/scripts/, add the card
-- to a dashboard, and point it at a climate entity.

local control = require "control"
local schedule = require "schedule"
local climate_lib = require "climate"
local card = require("card").new { kind = "enhanced_climate" }

-- Seed value (°C) for an enhanced climate's override temperature — the setpoint
-- a timed override drives it to — until the user edits it via the card.
local DEFAULT_OVERRIDE_TEMP = 23

-- Setpoint held while any bound window is open, clamped to the device's own min.
-- Writing a low setpoint is how a self-contained controller PAUSES heating:
-- skipping the write would leave the device coasting at its last target.
local FROST_TEMP = 15

-- Per climate entity, the last companion payload written and when, so an
-- unchanged sensor is not rewritten every minute. PUBLISH_HEARTBEAT bounds how
-- long it goes unrefreshed, so it still self-heals after an HA restart drops it.
local published = {}
local PUBLISH_HEARTBEAT = 5 * 60

-- ---------------------------------------------------------------------------
-- Registry (§7.1): the enhanced climates, keyed by climate entity id. Global and
-- under one key, so the whole set round-trips as a single value that both the
-- control tick and the Ingress removal page iterate.
-- ---------------------------------------------------------------------------

local REGISTRY_KEY = "enhanced_climate:registry"

local function load_registry()
  local reg = global.get(REGISTRY_KEY)
  if type(reg) ~= "table" then return {} end
  return reg
end

local function save_registry(reg)
  global.set(REGISTRY_KEY, reg)
end

local function is_registered(climate_entity)
  return type(climate_entity) == "string" and load_registry()[climate_entity] ~= nil
end

-- ---------------------------------------------------------------------------
-- Per-climate dynamic state lives in this script's own KV store, keyed by
-- climate entity id.
-- ---------------------------------------------------------------------------

local function sched_key(climate) return "schedule:" .. climate end
local function override_key(climate) return "override:" .. climate end
local function manual_key(climate) return "manual:" .. climate end
local function override_temp_key(climate) return "override_temp:" .. climate end
local function desired_key(climate) return "desired:" .. climate end
-- What the controller last COMMANDED, as against what the user asked for. The
-- two differ once the overshoot correction cuts a warmup short
-- (overshoot-spec.md §7), and anything comparing against the value on the
-- device must read this one or it reads our own correction as a dial nudge.
local function written_key(climate) return "written:" .. climate end
local function restore_key(climate) return "restore:" .. climate end

-- climate.living_room -> living_room, so the card can derive the companion id
-- rather than having it configured.
local function slug_of(climate)
  return (climate:gsub("^climate%.", ""))
end

local now_parts, parse_time = climate_lib.now_parts, climate_lib.parse_time

local function override_temp(climate)
  local value = store.get(override_temp_key(climate))
  if type(value) == "number" then return value end
  return DEFAULT_OVERRIDE_TEMP
end

local mode, current_target = climate_lib.mode, climate_lib.target
local temp_bounds = climate_lib.bounds

-- The entity's friendly_name, falling back to the id while it has none.
local function friendly_name(climate)
  local state = ha.get_state(climate)
  if state and state.attributes and type(state.attributes.friendly_name) == "string" then
    return state.attributes.friendly_name
  end
  return climate
end

local function load_schedule(climate)
  local stored = store.get(sched_key(climate))
  if type(stored) == "table" and type(stored.days) == "table" then return stored.days end
  return {}
end

-- The window sensors the card bound to this climate at configure time.
local function window_sensors_of(climate)
  local cfg = load_registry()[climate]
  if cfg == nil or type(cfg.window_sensors) ~= "table" then return {} end
  return cfg.window_sensors
end

-- Open if ANY bound sensor reads "on"; an unseeded one counts as closed.
local function window_open(climate)
  local states = {}
  for _, sensor in ipairs(window_sensors_of(climate)) do
    local state = ha.get_state(sensor)
    states[#states + 1] = state and state.state or "off"
  end
  return control.window_open(states)
end

-- Suppresses manual-change detection until every bound sensor has seeded.
local function window_unknown(climate)
  for _, sensor in ipairs(window_sensors_of(climate)) do
    if ha.get_state(sensor) == nil then return true end
  end
  return false
end

-- The live timed override, or nil, clearing an expired one so the climate
-- reverts to schedule.
local function active_override(climate, now)
  local override = store.get(override_key(climate))
  if type(override) ~= "table" or not override.active or type(override.ends_at) ~= "string" then
    return nil
  end
  local ends = parse_time(override.ends_at)
  if ends == nil then
    store.delete(override_key(climate))
    return nil
  end
  if now:before(ends) then return override end
  store.delete(override_key(climate))
  return nil
end

-- The live manual hold, or nil, clearing it once `expires` has passed. ("until"
-- would be a Lua keyword.)
local function active_manual(climate, now)
  local manual = store.get(manual_key(climate))
  if type(manual) ~= "table" or type(manual.temp) ~= "number" or type(manual.expires) ~= "string" then
    return nil
  end
  local exp = parse_time(manual.expires)
  if exp == nil then
    store.delete(manual_key(climate))
    return nil
  end
  if now:before(exp) then return manual end
  store.delete(manual_key(climate))
  return nil
end

-- override > manual > schedule via control.desired, resolving each source's
-- candidate and expiring stale holds on the way.
local function desired(climate, now, dow, minute)
  local override = active_override(climate, now) and override_temp(climate) or nil
  local manual = active_manual(climate, now)
  local sched_temp = schedule.resolve(load_schedule(climate), dow, minute)
  return control.desired(override, manual and manual.temp or nil, sched_temp)
end

local function set_temp(climate, temp)
  ha.log("info", "set_temperature " .. climate .. " = " .. tostring(temp) .. "°")
  ha.call_service("climate", "set_temperature", { entity_id = climate, temperature = temp })
end

-- Writes the companion entity (§6) the card reads: state is the desired setpoint
-- when controlled, else "off", with the schedule, override, manual, window and
-- preset detail in its attributes. desired_temp arrives already clamped.
local function publish_companion(climate, now, desired_temp)
  local cfg = load_registry()[climate]
  if cfg == nil then return end
  local lo, hi = temp_bounds(climate)
  local friendly = friendly_name(climate)

  local override_tbl = { active = false }
  local override = active_override(climate, now)
  if override then
    override_tbl = { active = true, expires = override.ends_at, temp = override_temp(climate) }
  end

  local manual_tbl = { active = false }
  local manual = active_manual(climate, now)
  if manual then
    manual_tbl = { active = true, ["until"] = manual.expires } -- "until" is a keyword
  end

  local controlled = desired_temp ~= nil
  local state_value = controlled and desired_temp or "off"

  local attrs = {
    ha_lua_climate = climate,
    friendly_name = friendly,
    schedule = load_schedule(climate),
    override = override_tbl,
    override_temp = override_temp(climate), -- always surfaced so the card can edit it
    manual = manual_tbl,
    window = { sensors = window_sensors_of(climate), open = window_open(climate) },
    presets = cfg.presets,
    min_temp = lo,
    max_temp = hi,
    controlled = controlled,
    unit_of_measurement = "°C",
    device_class = "temperature",
    icon = "mdi:thermostat",
    removal = "Deleting the card keeps this running — remove it in the ha-lua panel",
  }

  -- Every set_state is a state_changed event and a recorder row, so an unchanged
  -- sensor is not rewritten each minute. The slow heartbeat still re-publishes
  -- unchanged data, because these states are not integration-backed and an HA
  -- restart drops them; a blind periodic write beats probing for our own sensor.
  local snapshot = json.encode({ state = state_value, attrs = attrs })
  local prev = published[climate]
  if prev and prev.snapshot == snapshot and (os.time() - prev.at) < PUBLISH_HEARTBEAT then
    return
  end

  -- set_state is non-raising, so an outage is invisible unless logged here.
  local created, err = card.publish(slug_of(climate), state_value, attrs)
  if err then
    ha.log("warn", "publish companion for " .. climate .. " failed: " .. err)
  else
    published[climate] = { snapshot = snapshot, at = os.time() }
    if created then
      ha.log("info", "published companion sensor for " .. climate)
    else
      ha.log("debug", "refreshed companion for " .. climate .. " (state " .. tostring(state_value) .. ")")
    end
  end
end

-- The per-climate control step: compute the desired setpoint, clamp it, remember
-- it, and write it when the shared gate allows. While any bound window is open
-- the frost setpoint is held instead, and the remembered desired is restored
-- once they all close. Configure, tick and every mutation refresh the companion
-- through this one path.
--
-- Two values are remembered, not one: `desired` is what was asked for, `written`
-- is what the device was commanded to. Manual detection compares against
-- `written`.
local function apply_climate(climate, now, dow, minute)
  local desired_temp, source = desired(climate, now, dow, minute)
  -- The pre-boost snapshot matters only while the boost is the active source:
  -- anything else takes the climate over from here.
  local previous = nil
  if source ~= "override" then
    previous = store.get(restore_key(climate))
    if previous ~= nil then store.delete(restore_key(climate)) end
  end
  local lo, hi = temp_bounds(climate)
  if desired_temp ~= nil then
    desired_temp = control.clamp_bounds(desired_temp, lo, hi)
    store.set(desired_key(climate), desired_temp)
    local commanded_temp = desired_temp
    if mode(climate) == "heat" then
      local current = current_target(climate)
      if window_open(climate) then
        commanded_temp = control.clamp_bounds(FROST_TEMP, lo, hi)
        if current == nil or math.abs(current - commanded_temp) > 0.05 then
          set_temp(climate, commanded_temp)
        end
      elseif control.should_write("heat", false, current, commanded_temp) then
        set_temp(climate, commanded_temp)
      end
    end
    store.set(written_key(climate), commanded_temp)
  else
    -- A boost ending with no schedule or hold under it must still put the dial
    -- back where it found it, or the boost temperature sticks forever.
    local restored = type(previous) == "number" and control.clamp_bounds(previous, lo, hi) or nil
    if restored and control.should_write(mode(climate), false, current_target(climate), restored) then
      set_temp(climate, restored)
      -- Our own write must not read as a dial nudge, and the detector compares
      -- against `written`.
      store.set(desired_key(climate), restored)
      store.set(written_key(climate), restored)
    else
      store.delete(desired_key(climate)) -- not controlled (no schedule/override/manual)
      store.delete(written_key(climate))
    end
  end
  publish_companion(climate, now, desired_temp)
end

-- Shared by the tick and the load-time resume, so both take one path.
local function apply_all(now, dow, minute)
  for climate_entity in pairs(load_registry()) do
    apply_climate(climate_entity, now, dow, minute)
  end
end

-- Override/manual expiry happens inside desired().
local function tick()
  apply_all(now_parts())
end

ha.every("1m", tick)

-- ---------------------------------------------------------------------------
-- Manual setpoint change detection (§7.2): this controller is the only thing that
-- writes the setpoint, and always writes what it recorded as `written`, so a
-- target differing from that is the user at the dial. It becomes an ad-hoc manual hold
-- lasting until the next schedule transition. One wildcard handler, because
-- climates are registered at runtime and a load-time registration cannot see
-- them.
-- ---------------------------------------------------------------------------

ha.on_state_change("climate.*", function(data)
  local climate_entity = data.entity_id
  if not is_registered(climate_entity) then return end
  local new_state = data.new_state
  if new_state == nil or new_state.attributes == nil then return end
  if new_state.state ~= "heat" then return end
  local target = new_state.attributes.temperature
  if type(target) ~= "number" then return end

  local now, dow, minute = now_parts()
  if active_override(climate_entity, now) then return end -- override wins; ignore nudges
  -- A window open (or not yet seeded) means we may have written the frost
  -- setpoint, which must not be mistaken for a user dial change.
  if window_open(climate_entity) or window_unknown(climate_entity) then return end

  local last_written = store.get(written_key(climate_entity))
  if not control.is_manual(target, last_written) then return end

  local _, _, mins_to_next = schedule.resolve(load_schedule(climate_entity), dow, minute)
  local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
  store.set(manual_key(climate_entity), {
    temp = target,
    expires = now:add(hold):format(time.RFC3339),
  })
  ha.log("info", "manual change on " .. climate_entity .. " -> " .. tostring(target) .. "° (held to next transition)")
  apply_climate(climate_entity, now, dow, minute) -- republish the new desired immediately
end)

-- A bound window opening or closing re-applies its climate within seconds rather
-- than at the next tick. Wildcard for the same reason as above.
ha.on_state_change("binary_sensor.*", function(data)
  local sensor = data.entity_id
  if type(sensor) ~= "string" then return end
  local now, dow, minute = now_parts()
  for climate_entity in pairs(load_registry()) do
    for _, bound in ipairs(window_sensors_of(climate_entity)) do
      if bound == sensor then
        ha.log("info", "window " .. sensor .. " (" .. tostring(data.new_state and data.new_state.state) ..
          ") changed -> re-applying " .. climate_entity)
        apply_climate(climate_entity, now, dow, minute)
        break
      end
    end
  end
end)

-- ---------------------------------------------------------------------------
-- Command handlers (card → daemon, §5). Each validates, mutates only when valid,
-- then re-applies the climate; a rejected command leaves state unchanged and the
-- card snaps back from the next hass update.
-- ---------------------------------------------------------------------------

-- Defaults the optional lists, so later code never type-checks them.
local function normalize(data)
  return {
    climate_entity = data.climate_entity,
    window_sensors = type(data.window_sensors) == "table" and data.window_sensors or {},
    presets = type(data.presets) == "table" and data.presets or {},
  }
end

local function list_equal(a, b)
  if type(a) ~= "table" or type(b) ~= "table" then return a == b end
  if #a ~= #b then return false end
  for i = 1, #a do
    if a[i] ~= b[i] then return false end
  end
  return true
end

-- So configure can no-op: a card re-firing on every tab focus must not thrash
-- the registry.
local function config_equal(x, y)
  if x == nil or y == nil then return x == y end
  return x.climate_entity == y.climate_entity
      and list_equal(x.window_sensors, y.window_sensors)
      and list_equal(x.presets, y.presets)
end

-- Idempotent upsert, fired by the card on load and on any config change.
card.on("configure", function(data)
  if type(data) ~= "table" or type(data.climate_entity) ~= "string" or data.climate_entity == "" then
    return
  end
  local cfg = normalize(data)
  local reg = load_registry()
  if not config_equal(reg[cfg.climate_entity], cfg) then
    reg[cfg.climate_entity] = cfg
    save_registry(reg)
    ha.log("info", "configure " .. cfg.climate_entity ..
      " (windows: " .. #cfg.window_sensors .. ", presets: " .. #cfg.presets .. ")")
  end
  -- Republish even when the config was unchanged: the card sends configure
  -- precisely when it does NOT see a matching companion, so clearing the dedup
  -- cache is what makes the companion reappear and ends the card's reconcile
  -- loop instead of it re-sending configure forever.
  published[cfg.climate_entity] = nil
  local now, dow, minute = now_parts()
  apply_climate(cfg.climate_entity, now, dow, minute) -- start controlling at once
end)

-- Deprovisions an enhanced climate, for both the card's remove command and the
-- Ingress removal page.
local function remove_climate(climate)
  if type(climate) ~= "string" then return end
  local reg = load_registry()
  if reg[climate] == nil then return end
  reg[climate] = nil
  save_registry(reg)
  store.delete(desired_key(climate))
  store.delete(written_key(climate))
  store.delete(restore_key(climate)) -- a re-add must not resurrect a pre-boost setpoint
  published[climate] = nil -- a re-add must re-publish, not skip against the stale cache
  local _, err = card.remove(slug_of(climate)) -- the companion disappears with it
  if err then
    ha.log("warn", "remove companion for " .. climate .. " failed: " .. err)
  else
    ha.log("info", "removed enhanced climate " .. climate)
  end
end

-- Deleting the card does NOT fire this: removal is deliberately explicit (§8).
card.on("remove", function(data)
  if type(data) ~= "table" then return end
  remove_climate(data.climate_entity)
end)

-- Replaces the 7-day schedule, bounded by the device's range.
card.on("schedule", function(data)
  local climate = data.climate_entity
  if not is_registered(climate) then return end
  local lo, hi = temp_bounds(climate)
  if not schedule.validate(data.schedule, lo, hi) then return end
  store.set(sched_key(climate), { days = data.schedule })
  ha.log("info", "schedule updated for " .. climate)
  local now, dow, minute = now_parts()
  apply_climate(climate, now, dow, minute)
end)

-- override starts or cancels a timed override (a boost to the override temp).
card.on("override", function(data)
  local climate = data.climate_entity
  if not is_registered(climate) then return end
  local now, dow, minute = now_parts()
  if data.cancel then
    store.delete(override_key(climate))
    ha.log("info", "override cancelled for " .. climate)
  else
    if type(data.minutes) ~= "number" or data.minutes <= 0 or data.minutes > 1440 then return end
    -- Once per boost, before it is overwritten: extending a running boost must
    -- not snapshot the boost temperature as the way back.
    if not active_override(climate, now) then
      local current = current_target(climate)
      if type(current) == "number" then store.set(restore_key(climate), current) end
    end
    store.set(override_key(climate), {
      active = true,
      ends_at = now:add(data.minutes * 60):format(time.RFC3339),
    })
    store.delete(manual_key(climate)) -- an override outranks and clears any manual hold
    -- The tick would notice up to a minute late, which the card shows as a
    -- countdown frozen at 00:00 and a boost that refuses to finish. The tick
    -- stays as the backstop for an ha.after lost to a restart.
    ha.after(string.format("%gm", data.minutes), function()
      local ends_now, ends_dow, ends_minute = now_parts()
      apply_climate(climate, ends_now, ends_dow, ends_minute)
    end)
    ha.log("info", "override " .. data.minutes .. "m for " .. climate)
  end
  apply_climate(climate, now, dow, minute)
end)

-- Edits the temperature a boost jumps to, bounded by the device's range.
card.on("settings", function(data)
  local climate = data.climate_entity
  if not is_registered(climate) then return end
  local lo, hi = temp_bounds(climate)
  if type(data.override_temp) ~= "number" or data.override_temp < lo or data.override_temp > hi then
    return
  end
  store.set(override_temp_key(climate), data.override_temp)
  ha.log("info", "override_temp set to " .. data.override_temp .. "° for " .. climate)
  local now, dow, minute = now_parts()
  apply_climate(climate, now, dow, minute) -- if an override is active, the new temp applies now
end)

-- ---------------------------------------------------------------------------
-- Ingress removal page (§8). An enhanced climate outlives any card, so removal
-- lives here: a page listing the registry with a remove button, covering both
-- deliberate teardown and orphans, since a deleted card cannot send remove.
-- ---------------------------------------------------------------------------

local JSON_HDR = { ["Content-Type"] = "application/json" }
local TEXT_HDR = { ["Content-Type"] = "text/plain" }

-- The registry as a flat array for the page.
local function list_climates()
  local out = {}
  for climate, cfg in pairs(load_registry()) do
    out[#out + 1] = {
      climate_entity = climate,
      name = friendly_name(climate),
      window_sensors = cfg.window_sensors or {},
      presets = cfg.presets or {},
    }
  end
  return out
end

ha.serve("GET", "/api/list", function()
  return 200, json.encode({ climates = list_climates() }), JSON_HDR
end)

ha.serve("POST", "/api/remove", function(req)
  local ok, body = pcall(json.decode, req.body)
  if not ok or type(body) ~= "table" or type(body.climate_entity) ~= "string" then
    return 400, "invalid JSON body", TEXT_HDR
  end
  remove_climate(body.climate_entity)
  return 200, json.encode({ climates = list_climates() }), JSON_HDR
end)

local PAGE = assert(fs.read("enhanced_climate.html"),
  "enhanced_climate.html missing next to enhanced_climate.lua")

ha.ui("Climate")
ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

-- Re-publish at load, before the first tick: an HA restart drops REST-set states,
-- so this is what makes the companions reappear.
do
  local count = 0
  for _ in pairs(load_registry()) do count = count + 1 end
  ha.log("info", "enhanced_climate loaded, resuming " .. count .. " climate(s)")
end
apply_all(now_parts())

ha.on_exception(ha.exceptions.log_file("enhanced-climate-errors.log"))
