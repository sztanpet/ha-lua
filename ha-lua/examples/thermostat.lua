-- thermostat.lua
--
-- The heating controller. It owns the schedule / override / manual
-- dimension of each zone's setpoint; heating_windows.lua owns the window
-- dimension. The split is clean: this script computes one desired setpoint per
-- zone, publishes it to global so the window script can restore it, and writes
-- it to the climate entity only while no window in the zone is open.
--
-- See thermostat-ui-spec.md for the full design. This file is the controller
-- (the 1-minute tick + desired() engine + manual-change detection); the HTTP
-- API and the single-page UI are added on top of it.

local zones = require "zones"
local schedule = require "schedule"
local control = require "control"

local zone_defs = zones.zones

-- Per-zone store keys. Schedule, the UI override (timed), the manual hold
-- (dial-detected) and the override setpoint all live in this script's KV store;
-- the published desired lives in `global` (shared).
local function sched_key(zone) return "schedule:" .. zone end
local function override_key(zone) return "override:" .. zone end
local function manual_key(zone) return "manual:" .. zone end
local function override_temp_key(zone) return "override_temp:" .. zone end

-- The card display order is a single UI preference shared by every browser, so
-- it lives under one fixed key (not per-zone): an array of zone ids.
local ORDER_KEY = "zone_order"

-- now_parts returns the current time userdata plus the schedule's weekday
-- (0=Mon..6=Sun, converted from Go's Sunday-first weekday) and minute-of-day.
local function now_parts()
  local now = time.now()
  local dow = (now:weekday() + 6) % 7
  return now, dow, now:hour() * 60 + now:minute()
end

local function parse_time(text)
  if type(text) ~= "string" then return nil end
  local parsed = time.parse(time.RFC3339, text)
  return parsed -- nil on parse failure
end

-- override_temp returns the zone's UI-settable override setpoint (the
-- temperature an override drives the zone to), seeding the default the first
-- time before the user touches the stepper.
local function override_temp(zone)
  local value = store.get(override_temp_key(zone))
  if type(value) == "number" then return value end
  return zones.default_override_temp
end

-- mode returns the climate entity's hvac mode ("heat"/"off"/...) or nil if the
-- entity is not yet seeded.
local function mode(zone)
  local state = ha.get_state(zone_defs[zone].climate)
  if state == nil then return nil end
  return state.state
end

local function current_target(zone)
  local state = ha.get_state(zone_defs[zone].climate)
  if state and state.attributes then return state.attributes.temperature end
  return nil
end

-- temp_bounds returns the climate entity's accepted setpoint range. HA
-- advertises min_temp/max_temp on every climate entity, and writing a
-- temperature outside that range is silently rejected by HA (the setpoint just
-- stays put). We honour the device's own limits so the UI can never offer, nor
-- an override ever request, a value the device will refuse. Falls back to a
-- permissive 5..35 only while the entity has not seeded yet.
local function temp_bounds(zone)
  local lo, hi = 5, 35
  local state = ha.get_state(zone_defs[zone].climate)
  if state and state.attributes then
    if type(state.attributes.min_temp) == "number" then lo = state.attributes.min_temp end
    if type(state.attributes.max_temp) == "number" then hi = state.attributes.max_temp end
  end
  return lo, hi
end

-- any_window_open reports whether any window in the zone is definitely open.
-- A not-yet-seeded sensor (nil) counts as closed for the write decision. The
-- any-open/all-closed reduction itself is the shared control.window_open.
local function any_window_open(zone)
  local states = {}
  for _, window in ipairs(zone_defs[zone].windows) do
    local state = ha.get_state(window)
    states[#states + 1] = state and state.state or "off"
  end
  return control.window_open(states)
end

-- any_window_unknown reports whether any window sensor has no state yet. Used
-- only to suppress manual-change detection until the sensors have seeded.
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

-- active_override returns the live override table for the zone, or nil. An
-- expired override is cleared as a side effect so the zone reverts to schedule.
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

-- active_manual returns the live manual-hold table, or nil, clearing it once its
-- `expires` instant has passed. (We avoid the field name "until" because it is a
-- Lua keyword.)
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

-- desired implements §4.1: override beats manual beats schedule. Returns the
-- temperature and its source string ("override"/"manual"/"schedule"), or nil if
-- the zone has no schedule at all. The priority pick is the shared
-- control.desired; this wrapper resolves each source's candidate temperature
-- (and expires stale override/manual holds as a side effect).
local function desired(zone, now, dow, minute)
  local override = active_override(zone, now) and override_temp(zone) or nil
  local manual = active_manual(zone, now)
  local sched_temp = schedule.resolve(load_schedule(zone), dow, minute)
  return control.desired(override, manual and manual.temp or nil, sched_temp)
end

local function set_temp(zone, temp)
  ha.call_service("climate", "set_temperature", {
    entity_id = zone_defs[zone].climate,
    temperature = temp,
  })
end

-- apply_zone publishes the zone's desired setpoint and writes it to the climate
-- entity when the mode is heat and no window is open. The write is skipped when
-- the value is unchanged so we don't spam set_temperature.
local function apply_zone(zone, now, dow, minute)
  local desired_temp = desired(zone, now, dow, minute)
  if desired_temp == nil then return end
  global.set(zones.desired_key(zone), desired_temp)
  -- Write only in heat mode, with no window open (the window script's
  -- territory), and only when the value actually changed — the shared
  -- control.should_write gate.
  if control.should_write(mode(zone), any_window_open(zone), current_target(zone), desired_temp) then
    set_temp(zone, desired_temp)
  end
end

-- The single tick that drives everything (§8): recompute, publish and (maybe)
-- write every zone. Override/manual expiry is handled inside desired().
local function tick()
  local now, dow, minute = now_parts()
  for zone in pairs(zone_defs) do
    apply_zone(zone, now, dow, minute)
  end
end

ha.every("1m", tick)

-- Manual setpoint change detection (§9): the controller is the only thing that
-- writes `desired`, and it always writes exactly `desired`, so a climate target
-- that differs from the published desired is an external change by the user. It
-- becomes an ad-hoc manual hold that lasts until the next schedule transition.
for zone, conf in pairs(zone_defs) do
  ha.on_state_change(conf.climate, function(data)
    local new_state = data.new_state
    if new_state == nil or new_state.attributes == nil then return end
    if new_state.state ~= "heat" then return end
    local target = new_state.attributes.temperature
    if type(target) ~= "number" then return end

    local now, dow, minute = now_parts()
    if active_override(zone, now) then return end -- override wins; ignore dial nudges
    -- Window open or not-yet-seeded: that's the window script's 15°C territory.
    if any_window_open(zone) or any_window_unknown(zone) then return end

    local published = global.get(zones.desired_key(zone))
    -- Float tolerance: our own write (and the window restore) set target ==
    -- published exactly, but 21 vs 21.0 must not look like a manual change. The
    -- predicate is the shared control.is_manual.
    if not control.is_manual(target, published) then return end

    local _, _, mins_to_next = schedule.resolve(load_schedule(zone), dow, minute)
    local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
    store.set(manual_key(zone), {
      temp = target,
      expires = now:add(hold):format(time.RFC3339),
    })
    apply_zone(zone, now, dow, minute) -- republish the new desired immediately
  end)
end

-- ---------------------------------------------------------------------------
-- HTTP API (§6). Handlers run on this script's goroutine, so they may use any
-- ha.*/store.* call directly. All mutating endpoints re-apply the affected zone
-- and return the full state so the UI can refresh in one round-trip.
-- ---------------------------------------------------------------------------

local JSON_HDR = { ["Content-Type"] = "application/json" }
local TEXT_HDR = { ["Content-Type"] = "text/plain" }

local function json_ok(tbl)
  return 200, json.encode(tbl), JSON_HDR
end

local function bad(msg)
  return 400, msg, TEXT_HDR
end

-- zone_state builds the per-zone status block for GET /api/state.
local function zone_state(zone, now, dow, minute)
  local state = ha.get_state(zone_defs[zone].climate)
  local hvac_mode = state and state.state or "unknown"
  local current, target, hvac_action
  if state and state.attributes then
    current = state.attributes.current_temperature
    target = state.attributes.temperature
    -- hvac_action ("heating"/"idle"/...) is what the device is doing right
    -- now, distinct from the mode; the UI uses it to show "heating" vs "on".
    hvac_action = state.attributes.hvac_action
  end
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

-- ordered_zones returns the zone ids in the user-chosen display order. The
-- stored order is filtered to zones that still exist; any zone missing from it
-- (e.g. one newly added to zones.lua) is appended alphabetically. The result
-- therefore always covers exactly the current zone set, however stale the
-- stored order has become, so the UI can render straight from it.
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

-- PUT /api/order persists the card display order so every browser and user
-- sees the same arrangement. Body is { order = ["zone", ...] }; unknown ids are
-- rejected and duplicates collapsed. A partial list is accepted (ordered_zones
-- appends any omitted zones), so the UI may send only the zones it rendered.
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
  local zone = req.query and req.query.zone
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
-- The single-page UI (§7) lives in thermostat.html next to this script: one
-- self-contained HTML document (inline vanilla JS/CSS, no build step, no
-- external assets, all fetches RELATIVE so it works under both the stable LAN
-- port and the rotating ingress base path). It is read once at load via the
-- sandboxed fs module. Editing only the .html does not hot-reload — re-save
-- this .lua (the watcher watches .lua files) or restart the daemon.
-- ---------------------------------------------------------------------------

local PAGE = assert(fs.read("thermostat.html"),
  "thermostat.html missing next to thermostat.lua")

ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

-- Publish each zone's desired once at load time so the window script has a
-- value to restore before the first tick fires.
do
  local now, dow, minute = now_parts()
  for zone in pairs(zone_defs) do
    local desired_temp = desired(zone, now, dow, minute)
    if desired_temp ~= nil then global.set(zones.desired_key(zone), desired_temp) end
  end
end

ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/thermostat-errors.log"))
