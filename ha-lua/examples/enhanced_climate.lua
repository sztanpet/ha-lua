-- enhanced_climate.lua
--
-- A card-configured heating controller: each enhanced climate is one HA
-- `climate` entity wrapped with scheduling, timed overrides, manual-change
-- detection, and optional window cooperation — all provisioned at RUNTIME from
-- a Lovelace card (custom:ha-lua-enhanced-climate-card), never by editing this
-- file. See enhanced-climate-spec.md for the full design.
--
-- This is a NEW, standalone example, parallel to thermostat.lua (which keeps
-- its static lib/zones.lua model + Ingress editor). Both share the pure
-- lib/control.lua and lib/schedule.lua; only the wiring differs. Here the zone
-- definitions live in the card config, mirrored into the daemon over the
-- ha_lua_command event and surfaced back as companion sensor entities.
--
-- To use it: copy this file (and lib/) into /config/ha-lua/scripts/, add the
-- card to a dashboard, and point it at a climate entity.

local control = require "control"
local schedule = require "schedule"
local card = require("card").new { kind = "enhanced_climate" }

-- Seed value (°C) for an enhanced climate's override temperature — the setpoint
-- a timed override drives it to — until the user edits it via the card.
local DEFAULT_OVERRIDE_TEMP = 23

-- Setpoint (°C) held while any bound window is open, clamped to the device's
-- min on entities that won't accept it. Writing a low setpoint is how a
-- self-contained controller actually PAUSES heating (just skipping the write
-- would leave the device coasting at its last target); the desired is restored
-- once every window closes again.
local FROST_TEMP = 15

-- published caches, per climate entity, the JSON of the last companion payload
-- we actually wrote and when (os.time seconds). publish_companion consults it to
-- skip rewriting an unchanged sensor every minute; PUBLISH_HEARTBEAT bounds how
-- long an unchanged sensor goes without a refresh so it still self-heals after
-- an HA restart drops it.
local published = {}
local PUBLISH_HEARTBEAT = 5 * 60

-- ---------------------------------------------------------------------------
-- Registry (§7.1). The set of enhanced climates, keyed by climate entity id,
-- each { climate_entity, window_sensors, presets }. Global-scoped (shared)
-- under one namespaced key so the whole set round-trips as a single value that
-- the control tick and the Ingress removal page can both iterate.
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
-- Per-climate dynamic state (schedule / timed override / manual hold /
-- override temp / last-published desired) lives in this script's own KV store,
-- keyed by climate entity id.
-- ---------------------------------------------------------------------------

local function sched_key(e) return "schedule:" .. e end
local function override_key(e) return "override:" .. e end
local function manual_key(e) return "manual:" .. e end
local function override_temp_key(e) return "override_temp:" .. e end
local function desired_key(e) return "desired:" .. e end

-- slug_of derives the companion-sensor slug from a climate entity id:
-- climate.living_room -> living_room (so the card can derive the companion id
-- without it being configured).
local function slug_of(e)
  return (e:gsub("^climate%.", ""))
end

-- now_parts returns the current time userdata plus the schedule's weekday
-- (0=Mon..6=Sun, converted from Go's Sunday-first weekday) and minute-of-day.
local function now_parts()
  local now = time.now()
  local dow = (now:weekday() + 6) % 7
  return now, dow, now:hour() * 60 + now:minute()
end

local function parse_time(text)
  if type(text) ~= "string" then return nil end
  return time.parse(time.RFC3339, text) -- nil on parse failure
end

local function override_temp(e)
  local value = store.get(override_temp_key(e))
  if type(value) == "number" then return value end
  return DEFAULT_OVERRIDE_TEMP
end

local function mode(e)
  local state = ha.get_state(e)
  if state == nil then return nil end
  return state.state
end

local function current_target(e)
  local state = ha.get_state(e)
  if state and state.attributes then return state.attributes.temperature end
  return nil
end

-- temp_bounds returns the climate entity's accepted setpoint range. HA silently
-- drops a set_temperature outside min_temp/max_temp, so we honour the device's
-- own limits. Falls back to a permissive 5..35 while the entity has not seeded.
local function temp_bounds(e)
  local lo, hi = 5, 35
  local state = ha.get_state(e)
  if state and state.attributes then
    if type(state.attributes.min_temp) == "number" then lo = state.attributes.min_temp end
    if type(state.attributes.max_temp) == "number" then hi = state.attributes.max_temp end
  end
  return lo, hi
end

-- friendly_name resolves a climate entity's HA friendly_name, falling back to
-- the entity id itself while the entity has not seeded or carries no name.
local function friendly_name(e)
  local state = ha.get_state(e)
  if state and state.attributes and type(state.attributes.friendly_name) == "string" then
    return state.attributes.friendly_name
  end
  return e
end

local function load_schedule(e)
  local stored = store.get(sched_key(e))
  if type(stored) == "table" and type(stored.days) == "table" then return stored.days end
  return {}
end

-- window_sensors_of returns the bound window sensor ids for a climate (the list
-- the card stored at configure time), or an empty list.
local function window_sensors_of(e)
  local cfg = load_registry()[e]
  if cfg == nil or type(cfg.window_sensors) ~= "table" then return {} end
  return cfg.window_sensors
end

-- window_open reduces a climate's bound sensors to one boolean via the shared
-- control.window_open: open if ANY sensor reads "on", clear only when ALL are
-- closed. A not-yet-seeded sensor (nil) counts as closed.
local function window_open(e)
  local states = {}
  for _, sensor in ipairs(window_sensors_of(e)) do
    local state = ha.get_state(sensor)
    states[#states + 1] = state and state.state or "off"
  end
  return control.window_open(states)
end

-- window_unknown reports whether any bound sensor has not seeded yet. Used only
-- to suppress manual-change detection until the windows are known.
local function window_unknown(e)
  for _, sensor in ipairs(window_sensors_of(e)) do
    if ha.get_state(sensor) == nil then return true end
  end
  return false
end

-- active_override returns the live timed-override table, or nil, clearing an
-- expired one so the climate reverts to schedule.
local function active_override(e, now)
  local override = store.get(override_key(e))
  if type(override) ~= "table" or not override.active or type(override.ends_at) ~= "string" then
    return nil
  end
  local ends = parse_time(override.ends_at)
  if ends == nil then
    store.delete(override_key(e))
    return nil
  end
  if now:before(ends) then return override end
  store.delete(override_key(e))
  return nil
end

-- active_manual returns the live manual-hold table, or nil, clearing it once its
-- `expires` instant has passed. ("expires" not "until" — until is a keyword.)
local function active_manual(e, now)
  local manual = store.get(manual_key(e))
  if type(manual) ~= "table" or type(manual.temp) ~= "number" or type(manual.expires) ~= "string" then
    return nil
  end
  local exp = parse_time(manual.expires)
  if exp == nil then
    store.delete(manual_key(e))
    return nil
  end
  if now:before(exp) then return manual end
  store.delete(manual_key(e))
  return nil
end

-- desired picks override > manual > schedule via the shared control.desired,
-- resolving each source's candidate temperature (and expiring stale holds).
local function desired(e, now, dow, minute)
  local override = active_override(e, now) and override_temp(e) or nil
  local manual = active_manual(e, now)
  local sched_temp = schedule.resolve(load_schedule(e), dow, minute)
  return control.desired(override, manual and manual.temp or nil, sched_temp)
end

local function set_temp(e, temp)
  ha.log("info", "set_temperature " .. e .. " = " .. tostring(temp) .. "°")
  ha.call_service("climate", "set_temperature", { entity_id = e, temperature = temp })
end

-- publish_companion writes the sensor.ha_lua_enhanced_climate_<slug> companion
-- entity (§6) that the card reads: its state is the current desired setpoint
-- when controlled, else "off"; its attributes carry the schedule, override,
-- manual, window and preset detail plus the device range and identity markers.
-- desired_temp is the already-clamped value (or nil when not controlled).
local function publish_companion(e, now, desired_temp)
  local cfg = load_registry()[e]
  if cfg == nil then return end
  local lo, hi = temp_bounds(e)
  local friendly = friendly_name(e)

  local override_tbl = { active = false }
  local override = active_override(e, now)
  if override then
    override_tbl = { active = true, expires = override.ends_at, temp = override_temp(e) }
  end

  local manual_tbl = { active = false }
  local manual = active_manual(e, now)
  if manual then
    manual_tbl = { active = true, ["until"] = manual.expires } -- "until" is a keyword
  end

  local controlled = desired_temp ~= nil
  local state_value = controlled and desired_temp or "off"

  local attrs = {
    ha_lua_climate = e,
    friendly_name = friendly,
    schedule = load_schedule(e),
    override = override_tbl,
    override_temp = override_temp(e), -- always surfaced so the card can edit it
    manual = manual_tbl,
    window = { sensors = window_sensors_of(e), open = window_open(e) },
    presets = cfg.presets,
    min_temp = lo,
    max_temp = hi,
    controlled = controlled,
    unit_of_measurement = "°C",
    device_class = "temperature",
    icon = "mdi:thermostat",
    removal = "Deleting the card keeps this running — remove it in the ha-lua panel",
  }

  -- The 1-minute tick re-publishes every companion, but rewriting an unchanged
  -- sensor each minute is pure noise: every set_state is a state_changed event
  -- (and a recorder row). So skip the write when state+attrs match the last one
  -- we actually sent. We still re-publish unchanged data on a slow heartbeat so
  -- the sensor self-heals after an HA restart drops it (these states are not
  -- integration-backed). The heartbeat stays even though the daemon now prunes
  -- ghost entities from its mirror on reconnect: a blind periodic write is
  -- simpler and more robust than probing get_state for our own sensor.
  local snapshot = json.encode({ state = state_value, attrs = attrs })
  local prev = published[e]
  if prev and prev.snapshot == snapshot and (os.time() - prev.at) < PUBLISH_HEARTBEAT then
    return
  end

  -- set_state is non-raising, so log the result here: warn on failure (else an
  -- outage is invisible), info on first create, debug for the heartbeat refresh.
  local created, err = card.publish(slug_of(e), state_value, attrs)
  if err then
    ha.log("warn", "publish companion for " .. e .. " failed: " .. err)
  else
    published[e] = { snapshot = snapshot, at = os.time() }
    if created then
      ha.log("info", "published companion sensor for " .. e)
    else
      ha.log("debug", "refreshed companion for " .. e .. " (state " .. tostring(state_value) .. ")")
    end
  end
end

-- apply_climate is the per-climate control step: compute the desired setpoint,
-- clamp it to the device range, remember it (so manual detection can compare),
-- and write it when the shared gate allows. While any bound window is open the
-- climate is held at a frost setpoint instead — that is what pauses heating;
-- the desired (still remembered) is restored once every window closes. Every
-- pass re-publishes the companion, so configure / tick / mutation all refresh
-- it through this one path.
local function apply_climate(e, now, dow, minute)
  local desired_temp = desired(e, now, dow, minute)
  local lo, hi = temp_bounds(e)
  if desired_temp ~= nil then
    desired_temp = control.clamp_bounds(desired_temp, lo, hi)
    store.set(desired_key(e), desired_temp)
    if mode(e) == "heat" then
      local current = current_target(e)
      if window_open(e) then
        -- Pause: hold the frost setpoint while any window is open.
        local frost = control.clamp_bounds(FROST_TEMP, lo, hi)
        if current == nil or math.abs(current - frost) > 0.05 then
          set_temp(e, frost)
        end
      elseif control.should_write("heat", false, current, desired_temp) then
        set_temp(e, desired_temp)
      end
    end
  else
    store.delete(desired_key(e)) -- not controlled (no schedule/override/manual)
  end
  publish_companion(e, now, desired_temp)
end

-- apply_all runs the control step over every registered climate. Shared by the
-- 1-minute tick and the load-time resume so both go through one path.
local function apply_all(now, dow, minute)
  for climate_entity in pairs(load_registry()) do
    apply_climate(climate_entity, now, dow, minute)
  end
end

-- The 1-minute tick that drives every registered climate. Override/manual
-- expiry is handled inside desired().
local function tick()
  apply_all(now_parts())
end

ha.every("1m", tick)

-- ---------------------------------------------------------------------------
-- Manual setpoint change detection (§7.2): this controller is the only thing
-- that writes the desired, and it writes exactly the desired, so a climate
-- target that differs from the published desired is an external (user) change.
-- It becomes an ad-hoc manual hold lasting until the next schedule transition.
-- One wildcard handler covers every registered climate (they are added at
-- runtime, so a per-entity registration at load time can't see them).
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

  local published = store.get(desired_key(climate_entity))
  if not control.is_manual(target, published) then return end

  local _, _, mins_to_next = schedule.resolve(load_schedule(climate_entity), dow, minute)
  local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
  store.set(manual_key(climate_entity), {
    temp = target,
    expires = now:add(hold):format(time.RFC3339),
  })
  ha.log("info", "manual change on " .. climate_entity .. " -> " .. tostring(target) .. "° (held to next transition)")
  apply_climate(climate_entity, now, dow, minute) -- republish the new desired immediately
end)

-- Window immediacy: a bound window opening or closing re-applies the affected
-- climate(s) within seconds rather than waiting for the next 1-minute tick. One
-- wildcard handler covers sensors bound to climates added at runtime.
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
-- Command handlers (card → daemon, §5). Every handler validates, mutates only
-- when valid, then re-applies the climate; a rejected command leaves state
-- unchanged and the card snaps back from the next hass update (optimism-free).
-- ---------------------------------------------------------------------------

-- normalize coerces a configure payload into the stored shape, defaulting the
-- optional lists so later code never has to type-check them.
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

-- config_equal compares two stored configs so configure can no-op when nothing
-- changed (a card re-firing on every tab focus must not thrash the registry).
local function config_equal(x, y)
  if x == nil or y == nil then return x == y end
  return x.climate_entity == y.climate_entity
      and list_equal(x.window_sensors, y.window_sensors)
      and list_equal(x.presets, y.presets)
end

-- configure provisions (idempotent upsert) an enhanced climate. Fired by the
-- card on load / config change. Only mutates + re-applies when the effective
-- config actually changed.
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
  -- Always (re)publish the companion, even when the config was unchanged. The
  -- card sends configure precisely when it does NOT see a matching companion —
  -- e.g. HA dropped the non-integration entity on restart while our registry
  -- (and the publish dedup cache) still think it exists. Clearing the cache
  -- forces a write so the companion reappears and the card's reconcile loop
  -- terminates instead of re-sending configure forever.
  published[cfg.climate_entity] = nil
  local now, dow, minute = now_parts()
  apply_climate(cfg.climate_entity, now, dow, minute) -- start controlling at once
end)

-- remove_climate deprovisions an enhanced climate: drop it from the registry,
-- forget its desired, and remove the companion. Shared by the card's remove
-- command and the Ingress removal page so both go through one path.
local function remove_climate(e)
  if type(e) ~= "string" then return end
  local reg = load_registry()
  if reg[e] == nil then return end
  reg[e] = nil
  save_registry(reg)
  store.delete(desired_key(e))
  published[e] = nil -- a re-add must re-publish, not skip against the stale cache
  local _, err = card.remove(slug_of(e)) -- the companion disappears with it
  if err then
    ha.log("warn", "remove companion for " .. e .. " failed: " .. err)
  else
    ha.log("info", "removed enhanced climate " .. e)
  end
end

-- remove deprovisions an enhanced climate (also reachable from the Ingress page,
-- §8). Deleting the card does NOT fire this — removal is deliberately explicit.
card.on("remove", function(data)
  if type(data) ~= "table" then return end
  remove_climate(data.climate_entity)
end)

-- schedule replaces the 7-day schedule, bounded by the device's range.
card.on("schedule", function(data)
  local e = data.climate_entity
  if not is_registered(e) then return end
  local lo, hi = temp_bounds(e)
  if not schedule.validate(data.schedule, lo, hi) then return end
  store.set(sched_key(e), { days = data.schedule })
  ha.log("info", "schedule updated for " .. e)
  local now, dow, minute = now_parts()
  apply_climate(e, now, dow, minute)
end)

-- override starts or cancels a timed override (a boost to the override temp).
card.on("override", function(data)
  local e = data.climate_entity
  if not is_registered(e) then return end
  local now, dow, minute = now_parts()
  if data.cancel then
    store.delete(override_key(e))
    ha.log("info", "override cancelled for " .. e)
  else
    if type(data.minutes) ~= "number" or data.minutes <= 0 or data.minutes > 1440 then return end
    store.set(override_key(e), {
      active = true,
      ends_at = now:add(data.minutes * 60):format(time.RFC3339),
    })
    store.delete(manual_key(e)) -- an override outranks and clears any manual hold
    ha.log("info", "override " .. data.minutes .. "m for " .. e)
  end
  apply_climate(e, now, dow, minute)
end)

-- settings edits the override temperature (the target a boost jumps to),
-- bounded by the device's range.
card.on("settings", function(data)
  local e = data.climate_entity
  if not is_registered(e) then return end
  local lo, hi = temp_bounds(e)
  if type(data.override_temp) ~= "number" or data.override_temp < lo or data.override_temp > hi then
    return
  end
  store.set(override_temp_key(e), data.override_temp)
  ha.log("info", "override_temp set to " .. data.override_temp .. "° for " .. e)
  local now, dow, minute = now_parts()
  apply_climate(e, now, dow, minute) -- if an override is active, the new temp applies now
end)

-- ---------------------------------------------------------------------------
-- Ingress removal page (§8). An enhanced climate outlives any card, so removal
-- is explicit and lives here: a minimal page listing the registry with a remove
-- button. This covers deliberate teardown and orphans (a card deleted from a
-- dashboard can't send remove) alike. Served on this example's Ingress panel.
-- ---------------------------------------------------------------------------

local JSON_HDR = { ["Content-Type"] = "application/json" }
local TEXT_HDR = { ["Content-Type"] = "text/plain" }

-- list_climates returns the registry as a flat array for the page, resolving a
-- friendly name from the climate entity when one is available.
local function list_climates()
  local out = {}
  for e, cfg in pairs(load_registry()) do
    out[#out + 1] = {
      climate_entity = e,
      name = friendly_name(e),
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

ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

-- Re-publish every registered climate at load so the companions reappear after
-- a restart (REST-set states are dropped by an HA restart) before the first
-- tick — and resume controlling them.
do
  local count = 0
  for _ in pairs(load_registry()) do count = count + 1 end
  ha.log("info", "enhanced_climate loaded, resuming " .. count .. " climate(s)")
end
apply_all(now_parts())

ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/enhanced-climate-errors.log"))
