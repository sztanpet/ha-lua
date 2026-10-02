-- thermostat.lua
--
-- The heating controller: it owns the schedule / override / manual dimension of
-- each zone's setpoint, heating_windows.lua owns the window dimension. One
-- desired setpoint per zone is computed here, published to global so the window
-- script can restore it, and written to the climate entity only while no window
-- in the zone is open. See thermostat-ui-spec.md for the design.

local zones = require "zones"
local schedule = require "schedule"
local control = require "control"
local climate = require "climate"

local zone_defs = zones.zones

-- Schedule, timed override, manual hold and override setpoint live in this
-- script's KV store; the published desired lives in `global`, shared.
local function sched_key(zone) return "schedule:" .. zone end
local function override_key(zone) return "override:" .. zone end
local function manual_key(zone) return "manual:" .. zone end
local function override_temp_key(zone) return "override_temp:" .. zone end

-- One display order shared by every browser, so one fixed key, not per-zone.
local ORDER_KEY = "zone_order"

local now_parts, parse_time = climate.now_parts, climate.parse_time

-- The zone's climate entity, which every read below goes through.
local function entity_of(zone)
  return zone_defs[zone].climate
end

-- The temperature an override drives the zone to, seeded before the user first
-- touches the stepper.
local function override_temp(zone)
  local value = store.get(override_temp_key(zone))
  if type(value) == "number" then return value end
  return zones.default_override_temp
end

local function mode(zone) return climate.mode(entity_of(zone)) end
local function current_target(zone) return climate.target(entity_of(zone)) end
local function temp_bounds(zone) return climate.bounds(entity_of(zone)) end

-- Any window in the zone definitely open; an unseeded sensor counts as closed.
local function any_window_open(zone)
  local states = {}
  for _, window in ipairs(zone_defs[zone].windows) do
    local state = ha.get_state(window)
    states[#states + 1] = state and state.state or "off"
  end
  return control.window_open(states)
end

-- Suppresses manual-change detection until every window sensor has seeded.
local function any_window_unknown(zone)
  for _, window in ipairs(zone_defs[zone].windows) do
    if ha.get_state(window) == nil then return true end
  end
  return false
end

local function load_schedule(zone)
  local stored = store.get(sched_key(zone))
  if type(stored) == "table" and type(stored.days) == "table" then return stored.days end
  return {}
end

-- The live override, or nil. An expired one is cleared here, so the zone reverts
-- to schedule.
local function active_override(zone, now)
  local override = store.get(override_key(zone))
  if type(override) ~= "table" or not override.active or type(override.ends_at) ~= "string" then
    return nil
  end
  local ends = parse_time(override.ends_at)
  if ends == nil then
    store.delete(override_key(zone))
    return nil
  end
  if now:before(ends) then return override end
  store.delete(override_key(zone))
  return nil
end

-- The live manual hold, or nil, clearing it once `expires` has passed. ("until"
-- would be a Lua keyword.)
local function active_manual(zone, now)
  local manual = store.get(manual_key(zone))
  if type(manual) ~= "table" or type(manual.temp) ~= "number" or type(manual.expires) ~= "string" then
    return nil
  end
  local exp = parse_time(manual.expires)
  if exp == nil then
    store.delete(manual_key(zone))
    return nil
  end
  if now:before(exp) then return manual end
  store.delete(manual_key(zone))
  return nil
end

-- §4.1: override beats manual beats schedule. Resolves each source's candidate
-- for the shared control.desired, expiring stale holds on the way.
local function desired(zone, now, dow, minute)
  local override = active_override(zone, now) and override_temp(zone) or nil
  local manual = active_manual(zone, now)
  local sched_temp = schedule.resolve(load_schedule(zone), dow, minute)
  return control.desired(override, manual and manual.temp or nil, sched_temp)
end

local function set_temp(zone, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id = entity_of(zone),
    temperature = temp,
  })
end

-- Publishes the zone's setpoint and writes it to the climate entity when the
-- mode is heat and no window is open.
local function apply_zone(zone, now, dow, minute)
  local desired_temp = desired(zone, now, dow, minute)
  if desired_temp == nil then return end
  global.set(zones.desired_key(zone), desired_temp)
  global.set(zones.written_key(zone), desired_temp)
  if control.should_write(mode(zone), any_window_open(zone), current_target(zone), desired_temp) then
    set_temp(zone, desired_temp)
  end
end

-- The single tick that drives everything (§8). Override/manual expiry happens
-- inside desired().
local function tick()
  local now, dow, minute = now_parts()
  for zone in pairs(zone_defs) do
    apply_zone(zone, now, dow, minute)
  end
end

ha.every("1m", tick)

-- Manual setpoint change detection (§9): the controller is the only thing that
-- writes the setpoint, and always writes what it published as `written`, so a
-- target differing from that is the user at the dial. It becomes an ad-hoc
-- manual hold lasting until the next schedule transition.
-- Judges a climate state change as the user at the dial and records the hold.
-- Returns whether it did.
local function manual_change(zone, new_state, now, dow, minute)
  local target = new_state.attributes.temperature
  if type(target) ~= "number" then return false end
  if active_override(zone, now) then return false end -- an override outranks the dial
  -- Window open or unseeded: the window script's frost territory.
  if any_window_open(zone) or any_window_unknown(zone) then return false end

  local published = global.get(zones.written_key(zone))
  if not control.is_manual(target, published) then return false end

  local _, _, mins_to_next = schedule.resolve(load_schedule(zone), dow, minute)
  local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
  store.set(manual_key(zone), {
    temp = target,
    expires = now:add(hold):format(time.RFC3339),
  })
  return true
end

for zone, conf in pairs(zone_defs) do
  ha.on_state_change(conf.climate, function(data)
    local new_state = data.new_state
    if new_state == nil or new_state.attributes == nil then return end
    if new_state.state ~= "heat" then return end
    local now, dow, minute = now_parts()

    -- The dial is judged BEFORE anything writes: re-applying below can change
    -- the setpoint, and that write would read as a dial change against the
    -- target this very event carries.
    if manual_change(zone, new_state, now, dow, minute) then
      apply_zone(zone, now, dow, minute) -- republish at once
    end
  end)
end

-- ---------------------------------------------------------------------------
-- HTTP API (§6). Handlers run on this script's goroutine, so any ha.*/store.*
-- call is safe. Mutating endpoints re-apply the zone and return the full state,
-- so the UI refreshes in one round-trip.
-- ---------------------------------------------------------------------------

local JSON_HDR = { ["Content-Type"] = "application/json" }
local TEXT_HDR = { ["Content-Type"] = "text/plain" }

local function json_ok(tbl)
  return 200, json.encode(tbl), JSON_HDR
end

local function bad(msg)
  return 400, msg, TEXT_HDR
end

-- The per-zone status block for GET /api/state.
--
-- `target` is what the user asked for, `commanded` the number on the device,
-- read back from the climate entity rather than from what we published, so it
-- also shows the window script's frost value and exposes a write that never
-- landed.
local function zone_state(zone, now, dow, minute)
  local state = ha.get_state(entity_of(zone))
  local hvac_mode = state and state.state or "unknown"
  local current, commanded, hvac_action
  if state and state.attributes then
    current = state.attributes.current_temperature
    commanded = state.attributes.temperature
    -- What the device is doing now, distinct from the mode: "heating" vs "on".
    hvac_action = state.attributes.hvac_action
  end
  -- Computed rather than read from global, so a schedule transition shows at once
  -- instead of at the next tick. A zone with no schedule and no override has no
  -- request at all and falls back to the device value.
  local target = desired(zone, now, dow, minute) or commanded
  local days = load_schedule(zone)
  local sched_temp, now_index = schedule.resolve(days, dow, minute)
  local min_temp, max_temp = temp_bounds(zone)

  local override_tbl
  local override = active_override(zone, now)
  if override then
    local remaining = 0
    local ends = parse_time(override.ends_at)
    if ends ~= nil then
      local secs = ends:sub(now)
      if secs > 0 then remaining = math.floor(secs) end
    end
    override_tbl = { active = true, ends_at = override.ends_at, remaining_s = remaining }
  end

  return {
    mode = hvac_mode,
    hvac_action = hvac_action,
    current_temp = current,
    target = target,
    commanded = commanded,
    override_temp = override_temp(zone),
    min_temp = min_temp,
    max_temp = max_temp,
    window_open = any_window_open(zone),
    scheduled_temp = sched_temp,
    today = schedule.day_list(days, dow),
    now_index = now_index,
    override = override_tbl,
  }
end

-- The zone ids in the user-chosen display order: stored order filtered to zones
-- that still exist, then anything missing appended alphabetically. The result
-- always covers exactly the current zone set, however stale the stored order.
local function ordered_zones()
  local stored = store.get(ORDER_KEY)
  local seen, order = {}, {}
  if type(stored) == "table" then
    for _, zone in ipairs(stored) do
      if zone_defs[zone] ~= nil and not seen[zone] then
        seen[zone] = true
        order[#order + 1] = zone
      end
    end
  end
  local rest = {}
  for zone in pairs(zone_defs) do
    if not seen[zone] then rest[#rest + 1] = zone end
  end
  table.sort(rest)
  for _, zone in ipairs(rest) do order[#order + 1] = zone end
  return order
end

local function full_state()
  local now, dow, minute = now_parts()
  local zone_states = {}
  for zone in pairs(zone_defs) do
    zone_states[zone] = zone_state(zone, now, dow, minute)
  end
  return { zones = zone_states, order = ordered_zones() }
end

-- decode_body parses a JSON request body into a table, or returns nil.
local function decode_body(req)
  local ok, decoded = pcall(json.decode, req.body)
  if ok and type(decoded) == "table" then return decoded end
  return nil
end

ha.serve("GET", "/api/state", function()
  return json_ok(full_state())
end)

ha.serve("POST", "/api/override", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  if type(body.minutes) ~= "number" or body.minutes <= 0 or body.minutes > 1440 then
    return bad("minutes must be 1..1440")
  end
  local now, dow, minute = now_parts()
  store.set(override_key(zone), {
    active = true,
    ends_at = now:add(body.minutes * 60):format(time.RFC3339),
  })
  store.delete(manual_key(zone)) -- an override outranks and clears any manual hold
  apply_zone(zone, now, dow, minute)
  return json_ok(full_state())
end)

-- Registered after /api/override; the router's longest-prefix match sends
-- /api/override/cancel here and bare /api/override to the override handler.
ha.serve("POST", "/api/override/cancel", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  store.delete(override_key(zone))
  local now, dow, minute = now_parts()
  apply_zone(zone, now, dow, minute)
  return json_ok(full_state())
end)

ha.serve("PUT", "/api/settings", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  local lo, hi = temp_bounds(zone)
  if type(body.override_temp) ~= "number" or body.override_temp < lo or body.override_temp > hi then
    return bad(string.format("override_temp out of range (%g..%g)", lo, hi))
  end
  store.set(override_temp_key(zone), body.override_temp)
  local now, dow, minute = now_parts()
  apply_zone(zone, now, dow, minute) -- if an override is active, the new temp applies now
  return json_ok(full_state())
end)

-- Body is { order = ["zone", ...] }: unknown ids rejected, duplicates collapsed.
-- A partial list is fine, since ordered_zones appends whatever is omitted.
ha.serve("PUT", "/api/order", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  if type(body.order) ~= "table" then return bad("order must be an array") end
  local seen, clean = {}, {}
  for _, zone in ipairs(body.order) do
    if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone in order") end
    if not seen[zone] then
      seen[zone] = true
      clean[#clean + 1] = zone
    end
  end
  store.set(ORDER_KEY, clean)
  return json_ok(full_state())
end)

ha.serve("GET", "/api/schedule", function(req)
  local zone = req.query.zone
  if type(zone) == "string" and zone ~= "" then
    if zone_defs[zone] == nil then return bad("unknown zone") end
    return json_ok({ zone = zone, days = load_schedule(zone) })
  end
  local all = {}
  for other_zone in pairs(zone_defs) do
    all[other_zone] = load_schedule(other_zone)
  end
  return json_ok({ schedules = all })
end)

ha.serve("PUT", "/api/schedule", function(req)
  local body = decode_body(req)
  if body == nil then return bad("invalid JSON body") end
  local zone = body.zone
  if type(zone) ~= "string" or zone_defs[zone] == nil then return bad("unknown zone") end
  local lo, hi = temp_bounds(zone)
  local valid, msg = schedule.validate(body.days, lo, hi)
  if not valid then return bad("invalid schedule: " .. msg) end
  store.set(sched_key(zone), { days = body.days })
  local now, dow, minute = now_parts()
  apply_zone(zone, now, dow, minute)
  return json_ok(full_state())
end)

-- ---------------------------------------------------------------------------
-- The single-page UI (§7) is one self-contained HTML document in
-- thermostat.html: inline vanilla JS/CSS, no build step, and every fetch
-- RELATIVE so it works under both the LAN port and the rotating ingress base
-- path. Editing only the .html does not hot-reload — re-save this .lua.
-- ---------------------------------------------------------------------------

local PAGE = assert(fs.read("thermostat.html"),
  "thermostat.html missing next to thermostat.lua")

ha.ui("Heating")
ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

-- Publish at load so the window script has a value to restore, and the manual
-- detector one to compare against, before the first tick.
do
  local now, dow, minute = now_parts()
  for zone in pairs(zone_defs) do
    local desired_temp = desired(zone, now, dow, minute)
    if desired_temp ~= nil then
      global.set(zones.desired_key(zone), desired_temp)
      global.set(zones.written_key(zone), desired_temp)
    end
  end
end

ha.on_exception(ha.exceptions.log_file("thermostat-errors.log"))
