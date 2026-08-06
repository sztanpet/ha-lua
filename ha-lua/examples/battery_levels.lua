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
-- Add entity ids to IGNORE to drop the ones you do not care about.

-- How often the levels are sampled. Batteries move in whole percent steps over
-- days, so a coarse tick is plenty and keeps the series small. The page also
-- samples on every load, so opening it never shows a stale reading.
local SCAN_INTERVAL = "15m"

-- Entity ids never listed (phones you charge nightly, test devices, ...).
local IGNORE = {
  -- ["sensor.my_phone_battery_level"] = true,
}

-- The level a device is considered dead at. Some hardware stops reporting well
-- above zero; raise this if your sensors go silent at, say, 5%.
local EMPTY_LEVEL = 0

-- Guards before an ETA is shown at all. A straight line through two readings an
-- hour apart is astrology, not a forecast: demand a few real steps, spread over
-- a real span, adding up to a real drop.
local MIN_SAMPLES = 3
local MIN_SPAN = 6 * time.hour
local MIN_DROP = 2 -- percent

-- A rise of more than this many points means the pack was charged or swapped,
-- which makes every earlier sample worthless for the new run.
local RECHARGE_JUMP = 2

-- Cap on stored samples per entity. At one row per percent step this covers a
-- full 100→0 discharge; older rows fall off the front.
local MAX_SAMPLES = 120

-- Key holding the list of entity ids we have a series for, so series of
-- entities that leave Home Assistant can be cleaned up.
local TRACKED_KEY = "tracked"

local function series_key(entity_id) return "series:" .. entity_id end

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
    if not IGNORE[state.entity_id] then
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

-- drain_rate fits a least-squares line through the samples and returns its
-- slope in percent per second (negative while draining), or nil when the run is
-- too short, too flat, or rising. The current instant is appended as a point so
-- a battery that has not moved for a week drags the slope towards flat instead
-- of forever predicting the rate it drained at last month.
local function drain_rate(series, level, now_unix)
  local points = {}
  for _, sample in ipairs(series) do points[#points + 1] = sample end
  local newest = points[#points]
  if newest == nil then return nil end
  if newest.at < now_unix then points[#points + 1] = { at = now_unix, level = level } end

  if #points < MIN_SAMPLES then return nil end
  if points[#points].at - points[1].at < MIN_SPAN then return nil end
  if points[1].level - level < MIN_DROP then return nil end

  local sum_at, sum_level = 0, 0
  for _, point in ipairs(points) do
    sum_at = sum_at + point.at
    sum_level = sum_level + point.level
  end
  local mean_at, mean_level = sum_at / #points, sum_level / #points

  local covariance, variance = 0, 0
  for _, point in ipairs(points) do
    local offset = point.at - mean_at
    covariance = covariance + offset * (point.level - mean_level)
    variance = variance + offset * offset
  end
  if variance == 0 then return nil end

  local slope = covariance / variance
  if slope >= 0 then return nil end -- flat or climbing: nothing to predict
  return slope
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

-- forget_removed drops series for entities Home Assistant no longer has. An
-- entity that is merely unavailable keeps its history — a device offline for an
-- afternoon must not lose weeks of samples — so removal is judged by the entity
-- being gone from the state mirror entirely, not by it missing from this pass.
local function forget_removed(present)
  local tracked = {}
  for _, entity_id in ipairs(present) do tracked[entity_id] = true end

  local previous = store.get(TRACKED_KEY)
  if type(previous) == "table" then
    for _, entity_id in ipairs(previous) do
      if not tracked[entity_id] then
        if ha.get_state(entity_id) == nil then
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

-- Soonest to die first: entities with an ETA lead, ascending; the rest follow
-- by level, lowest first. Name breaks every tie so the order never wobbles
-- between polls.
local function by_urgency(left, right)
  if (left.eta_seconds ~= nil) ~= (right.eta_seconds ~= nil) then
    return left.eta_seconds ~= nil
  end
  if left.eta_seconds ~= nil and left.eta_seconds ~= right.eta_seconds then
    return left.eta_seconds < right.eta_seconds
  end
  if left.level ~= right.level then return left.level < right.level end
  return left.name < right.name
end

-- scan samples every battery and builds the page payload. It is both the timer
-- job and the API handler: sampling is idempotent (record only appends on a
-- real change), so an impatient browser refreshing the page costs nothing.
local function scan()
  local now = time.now()
  local now_unix = now:unix()
  local rows, present = {}, {}

  for _, battery in ipairs(batteries()) do
    present[#present + 1] = battery.entity_id
    local series = record(battery.entity_id, battery.level, now_unix)
    local slope = drain_rate(series, battery.level, now_unix)
    local eta_seconds
    if slope ~= nil and battery.level > EMPTY_LEVEL then
      eta_seconds = (battery.level - EMPTY_LEVEL) / -slope
    end
    local moved_at = changed_at(battery, series)
    rows[#rows + 1] = {
      entity_id = battery.entity_id,
      name = battery.name,
      level = battery.level,
      changed_ago = moved_at and (now_unix - moved_at) or nil,
      changed_at = moved_at,
      drain_per_day = slope and -slope * time.day or nil,
      eta_seconds = eta_seconds,
      empty_at = eta_seconds and now:add(eta_seconds):unix() or nil,
      samples = #series,
    }
  end

  forget_removed(present)
  table.sort(rows, by_urgency)
  return { generated_at = now:unix(), batteries = rows }
end

ha.every(SCAN_INTERVAL, function() scan() end)

local JSON_HDR = { ["Content-Type"] = "application/json" }

ha.serve("GET", "/api/state", function()
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
