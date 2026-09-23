-- battery_levels.lua
--
-- One page listing every battery in the house: current level, when that level
-- last changed, and an estimate of when it will hit empty — sorted so the
-- battery that dies first is at the top.
--
-- Home Assistant shows the level, not which of forty sensors is about to go
-- flat. That needs a drain RATE, and a rate needs weeks of history, while the
-- daemon's own history is purged after `retention_days`. So the series lives in
-- this script's KV store: one sample per observed level change, a handful of rows
-- a month per battery. Nothing here writes to Home Assistant.
--
-- Two kinds of entity are picked up with no configuration: `device_class:
-- battery` sensors, whose state IS the percentage, and anything carrying a
-- numeric `battery_level` attribute, whose state is something else. A battery you
-- do not care about can be ignored from the page: still listed, sorted last, no
-- longer sampled.

-- Batteries move in whole percent steps over days, so a coarse tick keeps the
-- series small. The page also samples on load, so it never shows a stale reading.
local SCAN_INTERVAL = "15m"

-- Most hardware goes flaky well above zero, so "empty" is not 0%.
local EMPTY_LEVEL = 15

-- Any shorter and one step extrapolates into a wild number.
local MIN_SPAN = 24 * time.hour

-- The coarsest granularity common hardware reports, so the floor below stays
-- true for the finer-grained ones too.
local COARSEST_STEP = 10

-- A rise of this many points above the run's low point is a charge or a swap.
-- Measured from the low, not the last sample: plenty of entities fluctuate a
-- few points a day, and a slow top-up never steps far enough at once.
local RECHARGE_RISE = 10

-- Cap on stored samples per entity. At one row per percent step this covers a
-- full 100→0 discharge; older rows fall off the front.
local MAX_SAMPLES = 120

-- Events kept per entity. Every jump in a forecast has a cause, but the cause is
-- a mutation of the series that has overwritten its own evidence by the time
-- anyone looks — hence a trail that is always recording.
local MAX_EVENTS = 40

-- Log a forecast that moved by more than this fraction with no sample behind
-- it. Below that it is just the window growing under a fixed drop.
local ETA_DRIFT = 0.10

-- Key holding the list of entity ids we have a series for, so series of
-- entities that leave Home Assistant can be cleaned up.
local TRACKED_KEY = "tracked"

-- Ignored batteries stay listed but are never sampled.
local IGNORED_KEY = "ignored"

local function series_key(entity_id) return "series:" .. entity_id end
local function events_key(entity_id) return "events:" .. entity_id end

-- Last forecast per entity, in memory only: it decides whether the next one is
-- worth writing down, and a restart re-baselining it costs one extra event.
local reported = {}

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

-- The percentage, plus whether it is the entity's own state. That flag matters
-- for "last changed": a battery sensor changes state only when the level moves,
-- while a device_tracker changes every time the phone does.
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

-- Every entity currently reporting a plausible percentage. An unavailable one
-- drops out of this pass with its stored series untouched.
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

local function lowest_level(series)
  local low = nil
  for _, entry in ipairs(series) do
    if low == nil or entry.level < low then low = entry.level end
  end
  return low
end

-- Appends a sample when the level moved, returning the series and what it did to
-- it. A flat reading is deliberately not stored: the dwell is implied by the
-- newest sample being old, and the fit adds the current instant itself.
local function record(entity_id, level, now_unix)
  local series = load_series(entity_id)
  local newest = series[#series]
  local change = { why = "first" }
  if newest ~= nil then
    if newest.level == level then return series, nil end
    local low = lowest_level(series)
    if level >= low + RECHARGE_RISE then
      change = { why = "reset", from = newest.level, low = low, wiped = #series }
      series = {}
    else
      change = { why = "sample", from = newest.level }
    end
  end
  series[#series + 1] = { at = now_unix, level = level }
  while #series > MAX_SAMPLES do
    table.remove(series, 1)
    change.trimmed = true
  end
  store.set(series_key(entity_id), series)
  return series, change
end

-- One line on an entity's trail, carrying the whole computation, so a forecast
-- can be checked against its own inputs long after the scan that produced it.
local function note(entity_id, entry)
  local events = store.get(events_key(entity_id))
  if type(events) ~= "table" then events = {} end
  events[#events + 1] = entry
  while #events > MAX_EVENTS do table.remove(events, 1) end
  store.set(events_key(entity_id), events)
end

-- The drain rate is the MEDIAN slope over the pairs of samples (Theil-Sen), not
-- the secant between the endpoints: plenty of sensors report a level that
-- breathes a point either way with temperature, and a secant then answers from
-- whichever side of the wobble each endpoint was caught on. A median needs better
-- than a quarter of the pairs to disagree before it moves at all.
--
-- Only pairs this far apart count. Half a day of a 0.25 %/day drain is an eighth
-- of a point, well under the granularity a sensor reports, so a closer pair
-- describes the day's temperature and nothing else. Such pairs also sit at
-- exactly zero slope in numbers large enough to land the median in the pile of
-- zeros and report a battery that never drains.
local MIN_PAIR_SPAN = 12 * time.hour

local function pairwise_rate(series)
  local slopes, every = {}, {}
  for older = 1, #series - 1 do
    for newer = older + 1, #series do
      local span = series[newer].at - series[older].at
      if span > 0 then
        local slope = (series[newer].level - series[older].level) / span
        every[#every + 1] = slope
        if span >= MIN_PAIR_SPAN then slopes[#slopes + 1] = slope end
      end
    end
  end
  -- A battery stepping twice inside MIN_PAIR_SPAN has no wide pair yet, and
  -- would otherwise get no rate at all.
  if #slopes == 0 then slopes = every end
  if #slopes == 0 then return nil end

  table.sort(slopes)
  local middle = math.floor(#slopes / 2)
  if #slopes % 2 == 1 then return slopes[middle + 1] end
  return (slopes[middle] + slopes[middle + 1]) / 2
end

-- Every pair above ends at a sample, so the measured rate says nothing about the
-- step in progress. The level has held for `dwell`, so it cannot be draining
-- faster than one granularity step per dwell whatever it did before. The bound is
-- loose right after a step and tightens from there, which is what stops a pack
-- that stopped moving from forecasting last month's rate forever.
local function dwell_cap(series, now_unix)
  local newest = series[#series]
  if newest == nil then return nil end
  local dwell = now_unix - newest.at
  if dwell <= 0 then return nil end

  -- The smallest step the sensor has taken is its granularity: 1 point for a
  -- phone, 10 for coarse hardware.
  local step = nil
  for index = 2, #series do
    local delta = math.abs(series[index].level - series[index - 1].level)
    if delta > 0 and (step == nil or delta < step) then step = delta end
  end
  if step == nil then return nil end
  return -step / dwell
end

-- MIN_SPAN is measured to NOW, not to the newest sample: the question is how long
-- we have been watching, which keeps its meaning when the level has sat still for
-- a week. Returns the rate plus the two numbers it was chosen between, so the
-- inspector can say where the answer came from.
local function drain_rate(series, now_unix)
  local oldest = series[1]
  local measured = pairwise_rate(series)
  local cap = dwell_cap(series, now_unix)

  local rate = nil
  if oldest ~= nil and now_unix - oldest.at >= MIN_SPAN
      and measured ~= nil and measured < 0 then
    rate = measured
    if cap ~= nil and cap > rate then rate = cap end -- both negative; flatter wins
  end
  return rate, measured, cap
end

-- A battery that never stepped has no rate, but six weeks at one level still
-- means it is not about to die.
local function lifetime_floor(remaining, moved_at, now_unix)
  if moved_at == nil then return nil end
  local dwell = now_unix - moved_at
  if dwell < MIN_SPAN then return nil end
  return remaining / COARSEST_STEP * dwell
end

-- When the reading last moved. Two sources disagree and the OLDER one is right:
-- HA's last_changed is exact but resets to "just now" on every HA restart, while
-- our newest sample is honest about the age but up to SCAN_INTERVAL late. An
-- attribute-only battery has no usable last_changed, and its first sample marks
-- when we started looking — nil, not "just now".
local function changed_at(battery, series)
  local newest = series[#series]
  local sampled = newest and newest.at or nil
  if not battery.level_is_state then
    if #series < 2 then return nil end
    return sampled
  end
  local parsed = time.parse(time.RFC3339, battery.last_changed or "")
  if parsed == nil then return sampled end
  local from_ha = parsed:unix()
  if sampled ~= nil and sampled < from_ha then return sampled end
  return from_ha
end

-- Drops the series of entities HA no longer has, and of the ones just ignored.
-- Removal is judged by absence from the state mirror, not from this pass: a
-- device offline for an afternoon must not lose weeks of samples.
local function forget_removed(present, ignored)
  local tracked = {}
  for _, entity_id in ipairs(present) do tracked[entity_id] = true end

  local previous = store.get(TRACKED_KEY)
  if type(previous) == "table" then
    for _, entity_id in ipairs(previous) do
      if not tracked[entity_id] then
        if ignored[entity_id] or ha.get_state(entity_id) == nil then
          store.delete(series_key(entity_id))
          store.delete(events_key(entity_id))
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

-- The whole derivation in one place: the page and the inspector must reach the
-- same numbers, or the inspector describes a calculation nobody ran.
local function forecast(battery, series, now_unix)
  local slope, measured, cap = drain_rate(series, now_unix)
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

  return { slope = slope, measured = measured, cap = cap,
           capped = slope ~= nil and cap ~= nil and measured ~= nil and cap > measured,
           moved_at = moved_at, remaining = remaining,
           eta_seconds = eta_seconds, eta_at_least = eta_at_least }
end

-- Which of the three answers the page will show. Crossing between them is the
-- loudest thing that can happen to a row, and it can happen with no sample
-- behind it: a span reaching MIN_SPAN turns "measuring" into a number on its own.
local function tier_of(eta_seconds, eta_at_least)
  if eta_seconds ~= nil then return "eta" end
  if eta_at_least ~= nil then return "floor" end
  return "none"
end

-- Three things earn a line on the trail: the series changed, the answer changed
-- kind, or the forecast moved further than the window's growth explains. The last
-- is the interesting one — the number swung while nothing visible happened.
local function trace(battery, series, change, fit, now_unix)
  local tier = tier_of(fit.eta_seconds, fit.eta_at_least)
  local previous = reported[battery.entity_id]
  reported[battery.entity_id] = { tier = tier, eta = fit.eta_seconds }

  if change == nil then
    if previous == nil then
      change = { why = "load" }
    elseif previous.tier ~= tier then
      change = { why = "tier", from_tier = previous.tier }
    elseif previous.eta ~= nil and fit.eta_seconds ~= nil
        and math.abs(fit.eta_seconds - previous.eta) > previous.eta * ETA_DRIFT then
      change = { why = "drift", from_eta = previous.eta }
    else
      return
    end
  end

  local oldest = series[1]
  note(battery.entity_id, {
    at = now_unix,
    why = change.why,
    from = change.from,
    from_tier = change.from_tier,
    from_eta = change.from_eta,
    low = change.low,
    wiped = change.wiped,
    trimmed = change.trimmed,
    level = battery.level,
    samples = #series,
    oldest_at = oldest and oldest.at or nil,
    oldest_level = oldest and oldest.level or nil,
    span = oldest and (now_unix - oldest.at) or nil,
    drop = oldest and (oldest.level - battery.level) or nil,
    per_day = fit.slope and -fit.slope * time.day or nil,
    capped = fit.capped,
    eta = fit.eta_seconds,
    floor = fit.eta_at_least,
  })
end

-- Samples every battery and builds the page payload. It is both the timer job and
-- the API handler: sampling is idempotent, so a browser refreshing costs nothing.
local function scan()
  local now = time.now()
  local now_unix = now:unix()
  local ignored = ignored_set()
  local rows, present = {}, {}

  for _, battery in ipairs(batteries()) do
    if ignored[battery.entity_id] then
      reported[battery.entity_id] = nil -- tracking it again starts a fresh trail
      rows[#rows + 1] = {
        entity_id = battery.entity_id,
        name = battery.name,
        level = battery.level,
        ignored = true,
        samples = 0,
      }
    else
      present[#present + 1] = battery.entity_id
      local series, change = record(battery.entity_id, battery.level, now_unix)
      local fit = forecast(battery, series, now_unix)
      trace(battery, series, change, fit, now_unix)

      rows[#rows + 1] = {
        entity_id = battery.entity_id,
        name = battery.name,
        level = battery.level,
        changed_ago = fit.moved_at and (now_unix - fit.moved_at) or nil,
        changed_at = fit.moved_at,
        drain_per_day = fit.slope and -fit.slope * time.day or nil,
        eta_seconds = fit.eta_seconds,
        eta_at_least = fit.eta_at_least,
        empty_at = fit.eta_seconds and now:add(fit.eta_seconds):unix() or nil,
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

-- Everything behind one battery's forecast: its samples, the arithmetic between
-- them and the answer, and the trail. Read-only on purpose — inspecting a
-- suspect row must not alter it.
ha.serve("GET", "/api/detail", function(req)
  local entity_id = (req.query or {}).entity_id
  if type(entity_id) ~= "string" or entity_id == "" then
    return 400, json.encode({ error = "entity_id required" }), JSON_HDR
  end
  local state = ha.get_state(entity_id)
  if state == nil then
    return 404, json.encode({ error = "unknown entity" }), JSON_HDR
  end
  local level, level_is_state = battery_level(state)
  if level == nil then
    return 404, json.encode({ error = "not a battery" }), JSON_HDR
  end

  local now_unix = time.now():unix()
  local series = load_series(entity_id)
  local events = store.get(events_key(entity_id))
  if type(events) ~= "table" then events = {} end

  local battery = {
    entity_id = entity_id,
    level = level,
    level_is_state = level_is_state,
    last_changed = state.last_changed,
  }
  local fit = forecast(battery, series, now_unix)
  local oldest, newest = series[1], series[#series]
  local low = lowest_level(series)

  return 200, json.encode({
    entity_id = entity_id,
    now = now_unix,
    level = level,
    level_is_state = level_is_state,
    ignored = ignored_set()[entity_id] or false,
    last_changed = state.last_changed,
    changed_at = fit.moved_at,
    series = series,
    events = events,
    -- Named after the constants they are compared against, so a row that says
    -- "measuring" can be read straight off: which guard failed, and by how much.
    math = {
      empty_level = EMPTY_LEVEL,
      min_span = MIN_SPAN,
      recharge_rise = RECHARGE_RISE,
      coarsest_step = COARSEST_STEP,
      max_samples = MAX_SAMPLES,
      remaining = fit.remaining,
      oldest_at = oldest and oldest.at or nil,
      oldest_level = oldest and oldest.level or nil,
      span = oldest and (now_unix - oldest.at) or nil,
      drop = oldest and (oldest.level - level) or nil,
      low = low,
      resets_at = low and low + RECHARGE_RISE or nil,
      -- The measured rate and the bound on it, separately: the page can only
      -- explain a forecast that stopped tracking the samples if it can see
      -- which of the two the answer came from.
      measured_per_day = fit.measured and -fit.measured * time.day or nil,
      cap_per_day = fit.cap and -fit.cap * time.day or nil,
      capped = fit.capped,
      dwell = newest and (now_unix - newest.at) or nil,
      pairs = #series * (#series - 1) / 2,
      per_day = fit.slope and -fit.slope * time.day or nil,
      eta_seconds = fit.eta_seconds,
      eta_at_least = fit.eta_at_least,
      tier = tier_of(fit.eta_seconds, fit.eta_at_least),
      samples = #series,
      steps = #series > 0 and #series - 1 or 0,
    },
  }), JSON_HDR
end)

-- Editing only the .html does not hot-reload: re-save this .lua, which is what
-- the watcher watches.
local PAGE = assert(fs.read("battery_levels.html"),
  "battery_levels.html missing next to battery_levels.lua")

ha.ui("Batteries")
ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

ha.on_exception(ha.exceptions.log_file("battery-levels-errors.log"))
