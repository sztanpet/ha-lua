-- battery_levels.lua
--
-- One page listing every battery in the house: current level, when that level
-- last changed, and an estimate of when it will hit empty — sorted so the
-- battery that dies first is at the top.
--
-- Home Assistant shows you the level. It does not tell you which of your forty
-- sensors is about to go flat, which is the only question worth asking before a
-- trip to the shop. That needs a drain RATE, and a rate needs history measured
-- in weeks — while the daemon's own state history is purged after
-- `retention_days` (2 by default). So this script keeps its own series in the
-- script KV store: one sample per observed level change, which for a battery is
-- a handful of rows a month. Nothing here writes to Home Assistant; it only
-- reads, samples, and serves a page.
--
-- Two kinds of entity are picked up automatically, with no configuration:
--   * `device_class: battery` sensors, whose state IS the percentage;
--   * anything carrying a numeric `battery_level` attribute (device_tracker,
--     vacuum, some locks), whose state is something else entirely.
-- The ones you do not care about (a phone you charge nightly, a test device)
-- can be ignored from the page: they stay listed, sorted last, but are no
-- longer sampled and get no forecast.

-- How often the levels are sampled. Batteries move in whole percent steps over
-- days, so a coarse tick is plenty and keeps the series small. The page also
-- samples on every load, so opening it never shows a stale reading.
local SCAN_INTERVAL = "15m"

-- Most hardware goes flaky well above zero, so "empty" is not 0%.
local EMPTY_LEVEL = 15

-- Any shorter and one step extrapolates into a wild number.
local MIN_SPAN = 24 * time.hour

-- The coarsest granularity common hardware reports, so the floor below stays
-- true for the finer-grained ones too.
local COARSEST_STEP = 10

-- A rise of more than this many points means the pack was charged or swapped,
-- which makes every earlier sample worthless for the new run.
local RECHARGE_JUMP = 2

-- Cap on stored samples per entity. At one row per percent step this covers a
-- full 100→0 discharge; older rows fall off the front.
local MAX_SAMPLES = 120

-- Key holding the list of entity ids we have a series for, so series of
-- entities that leave Home Assistant can be cleaned up.
local TRACKED_KEY = "tracked"

-- Ignored batteries stay listed but are never sampled.
local IGNORED_KEY = "ignored"

local function series_key(entity_id) return "series:" .. entity_id end

local function ignored_set()
  local ids = store.get(IGNORED_KEY)
  local set = {}
  if type(ids) == "table" then
    for _, entity_id in ipairs(ids) do set[entity_id] = true end
  end
  return set
end

local function save_ignored(set)
  local ids = {}
  for entity_id in pairs(set) do ids[#ids + 1] = entity_id end
  table.sort(ids)
  store.set(IGNORED_KEY, ids)
end

local function numeric(value)
  if type(value) == "number" then return value end
  if type(value) == "string" then return tonumber(value) end
  return nil
end

-- battery_level returns the entity's percentage plus whether that percentage is
-- the entity's own state. The flag matters for "last changed": a battery sensor
-- changes state only when the level moves, but a device_tracker carrying a
-- battery_level attribute changes state every time the phone moves, so its
-- last_changed says nothing about the battery.
local function battery_level(state)
  local attrs = state.attributes or {}
  if attrs.device_class == "battery" then
    local level = numeric(state.state)
    if level ~= nil then return level, true end
  end
  local level = numeric(attrs.battery_level)
  if level ~= nil then return level, false end
  return nil, false
end

-- batteries collects every entity currently reporting a plausible percentage.
-- Unavailable/unknown entities simply drop out for this pass (their state is
-- not numeric); their stored series is untouched.
local function batteries()
  local found = {}
  for _, state in ipairs(ha.get_entities("*")) do
    local level, level_is_state = battery_level(state)
    if level ~= nil and level >= 0 and level <= 100 then
      found[#found + 1] = {
        entity_id = state.entity_id,
        name = (state.attributes or {}).friendly_name or state.entity_id,
        level = level,
        level_is_state = level_is_state,
        last_changed = state.last_changed,
      }
    end
  end
  return found
end

local function load_series(entity_id)
  local series = store.get(series_key(entity_id))
  if type(series) ~= "table" then return {} end
  return series
end

-- record appends a sample when the level actually moved, and returns the
-- series. A flat reading is deliberately NOT stored: the dwell is implied by
-- "the newest sample is old", and the fit adds the current instant itself.
local function record(entity_id, level, now_unix)
  local series = load_series(entity_id)
  local newest = series[#series]
  if newest ~= nil then
    if newest.level == level then return series end
    if level > newest.level + RECHARGE_JUMP then series = {} end
  end
  series[#series + 1] = { at = now_unix, level = level }
  while #series > MAX_SAMPLES do table.remove(series, 1) end
  store.set(series_key(entity_id), series)
  return series
end

-- Ending the window at NOW rather than at the newest sample is what decays a
-- battery that stopped moving towards flat. One step suffices because the
-- oldest sample and the current level both start mid-dwell and those two errors
-- cancel; demanding more meant a coarse sensor forecast nothing for a year.
local function drain_rate(series, level, now_unix)
  local oldest = series[1]
  if oldest == nil then return nil end
  local drop = oldest.level - level
  local span = now_unix - oldest.at
  if drop <= 0 or span < MIN_SPAN then return nil end
  return -drop / span
end

-- A battery that never stepped has no rate, but six weeks at one level still
-- means it is not about to die.
local function lifetime_floor(remaining, moved_at, now_unix)
  if moved_at == nil then return nil end
  local dwell = now_unix - moved_at
  if dwell < MIN_SPAN then return nil end
  return remaining / COARSEST_STEP * dwell
end

-- changed_at returns the unix time the battery reading last moved. Two sources
-- disagree, and the OLDER one is right: Home Assistant's last_changed is exact
-- but resets to "just now" on every HA restart, while our newest sample is
-- honest about the age but up to SCAN_INTERVAL late. Entities that only carry
-- battery_level as an attribute have no usable last_changed at all, and their
-- first sample only marks when we started looking — nil, not "just now".
local function changed_at(battery, series)
  local newest = series[#series]
  local sampled = newest and newest.at or nil
  if not battery.level_is_state then
    if #series < 2 then return nil end
    return sampled
  end
  local parsed = time.parse(time.RFC3339, battery.last_changed or "")
  if parsed == nil then return sampled end
  local reported = parsed:unix()
  if sampled ~= nil and sampled < reported then return sampled end
  return reported
end

-- forget_removed drops series for entities Home Assistant no longer has, and
-- for the ones just ignored — ignoring means "stop tracking this", so the
-- samples go with it. An entity that is merely unavailable keeps its history —
-- a device offline for an afternoon must not lose weeks of samples — so removal
-- is judged by the entity being gone from the state mirror entirely, not by it
-- missing from this pass.
local function forget_removed(present, ignored)
  local tracked = {}
  for _, entity_id in ipairs(present) do tracked[entity_id] = true end

  local previous = store.get(TRACKED_KEY)
  if type(previous) == "table" then
    for _, entity_id in ipairs(previous) do
      if not tracked[entity_id] then
        if ignored[entity_id] or ha.get_state(entity_id) == nil then
          store.delete(series_key(entity_id))
        else
          tracked[entity_id] = true
        end
      end
    end
  end

  local ids = {}
  for entity_id in pairs(tracked) do ids[#ids + 1] = entity_id end
  table.sort(ids)
  store.set(TRACKED_KEY, ids)
end

-- Floors are systematically pessimistic, so one must never outrank a
-- measurement even when its number is the smaller of the two.
local function urgency_rank(row)
  if row.eta_seconds ~= nil then return 1, row.eta_seconds end
  if row.eta_at_least ~= nil then return 2, row.eta_at_least end
  return 3, row.level
end

-- Name breaks every tie so the order never wobbles between polls.
local function by_urgency(left, right)
  if not left.ignored ~= not right.ignored then return not left.ignored end
  local left_tier, left_key = urgency_rank(left)
  local right_tier, right_key = urgency_rank(right)
  if left_tier ~= right_tier then return left_tier < right_tier end
  if left_key ~= right_key then return left_key < right_key end
  if left.level ~= right.level then return left.level < right.level end
  return left.name < right.name
end

-- scan samples every battery and builds the page payload. It is both the timer
-- job and the API handler: sampling is idempotent (record only appends on a
-- real change), so an impatient browser refreshing the page costs nothing.
local function scan()
  local now = time.now()
  local now_unix = now:unix()
  local ignored = ignored_set()
  local rows, present = {}, {}

  for _, battery in ipairs(batteries()) do
    if ignored[battery.entity_id] then
      rows[#rows + 1] = {
        entity_id = battery.entity_id,
        name = battery.name,
        level = battery.level,
        ignored = true,
        samples = 0,
      }
    else
      present[#present + 1] = battery.entity_id
      local series = record(battery.entity_id, battery.level, now_unix)
      local slope = drain_rate(series, battery.level, now_unix)
      local moved_at = changed_at(battery, series)
      local remaining = battery.level - EMPTY_LEVEL

      local eta_seconds, eta_at_least
      if remaining <= 0 then
        eta_seconds = 0 -- due now, not unknown
      elseif slope ~= nil then
        eta_seconds = remaining / -slope
      else
        eta_at_least = lifetime_floor(remaining, moved_at, now_unix)
      end

      rows[#rows + 1] = {
        entity_id = battery.entity_id,
        name = battery.name,
        level = battery.level,
        changed_ago = moved_at and (now_unix - moved_at) or nil,
        changed_at = moved_at,
        drain_per_day = slope and -slope * time.day or nil,
        eta_seconds = eta_seconds,
        eta_at_least = eta_at_least,
        empty_at = eta_seconds and now:add(eta_seconds):unix() or nil,
        samples = #series,
        steps = #series - 1,
      }
    end
  end

  forget_removed(present, ignored)
  table.sort(rows, by_urgency)
  return { generated_at = now:unix(), batteries = rows }
end

ha.every(SCAN_INTERVAL, function() scan() end)

local JSON_HDR = { ["Content-Type"] = "application/json" }

ha.serve("GET", "/api/state", function()
  return 200, json.encode(scan()), JSON_HDR
end)

ha.serve("POST", "/api/ignore", function(req)
  local ok, body = pcall(json.decode, req.body)
  if not ok or type(body) ~= "table" or type(body.entity_id) ~= "string" then
    return 400, json.encode({ error = "entity_id required" }), JSON_HDR
  end
  if ha.get_state(body.entity_id) == nil then
    return 404, json.encode({ error = "unknown entity" }), JSON_HDR
  end

  local set = ignored_set()
  set[body.entity_id] = body.ignored and true or nil
  save_ignored(set)
  return 200, json.encode(scan()), JSON_HDR
end)

-- The page lives in battery_levels.html next to this script and is read once at
-- load through the sandboxed fs module. Editing only the .html does not
-- hot-reload — re-save this .lua (the watcher watches .lua files).
local PAGE = assert(fs.read("battery_levels.html"),
  "battery_levels.html missing next to battery_levels.lua")

ha.ui("Batteries")
ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

ha.on_exception(ha.exceptions.log_file("battery-levels-errors.log"))
