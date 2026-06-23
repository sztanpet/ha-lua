-- lib/schedule.lua
--
-- Pure schedule math, deliberately free of any ha.* / store.* / time access so
-- it can be unit-tested directly from Go. The controller does all the clock and
-- I/O work and calls into here with plain numbers.
--
-- A schedule is `days`: a table keyed by weekday string "0".."6" (0 = Monday,
-- 6 = Sunday — NOT Go's Sunday-first ordering), each value a list of
-- transitions { time = "HH:MM", temp = <number> }. The active temperature is
-- the most recent transition at or before "now"; before the first transition of
-- the day the last transition of the most recent non-empty earlier day carries
-- over.

local M = {}

-- parse_hhmm turns "HH:MM" into minutes since midnight, or nil if malformed.
function M.parse_hhmm(text)
  if type(text) ~= "string" then return nil end
  local hour, minute = text:match("^(%d%d):(%d%d)$")
  if hour == nil then return nil end
  hour, minute = tonumber(hour), tonumber(minute)
  if hour > 23 or minute > 59 then return nil end
  return hour * 60 + minute
end

-- day_list returns a copy of the transitions for lua weekday dow (0=Mon..6=Sun)
-- sorted ascending by time. Malformed-time rows sort to the front but are
-- otherwise harmless; resolve() ignores them.
function M.day_list(days, dow)
  local raw = days and days[tostring(dow)]
  if type(raw) ~= "table" then return {} end
  local out = {}
  for _, transition in ipairs(raw) do
    out[#out + 1] = transition
  end
  table.sort(out, function(a, b)
    return (M.parse_hhmm(a.time) or -1) < (M.parse_hhmm(b.time) or -1)
  end)
  return out
end

-- resolve walks the schedule for lua weekday dow (0=Mon..6=Sun) at minute-of-day
-- `minute` and returns three values:
--   active_temp     the temperature in effect now, or nil if the whole week is empty
--   now_index       0-based index into day_list(days, dow) of the active step,
--                   or -1 when the active value carried over from an earlier day
--   minutes_to_next minutes from now until the next transition (scanning forward
--                   up to 7 days), or nil if there are no transitions at all
function M.resolve(days, dow, minute)
  local today = M.day_list(days, dow)

  -- Most recent transition at or before `minute` today.
  local active, idx = nil, -1
  for i, transition in ipairs(today) do
    local trans_minutes = M.parse_hhmm(transition.time)
    if trans_minutes ~= nil and trans_minutes <= minute then
      active = transition.temp
      idx = i - 1
    end
  end

  -- Nothing yet today: carry the last transition of the most recent non-empty
  -- earlier day. Walk back up to 7 days so a run of empty days is handled.
  if active == nil then
    for back = 1, 7 do
      local list = M.day_list(days, (dow - back) % 7)
      if #list > 0 then
        active = list[#list].temp
        idx = -1
        break
      end
    end
  end

  -- Next transition strictly after now: the rest of today first, then scan
  -- forward up to 7 days for the first transition of the next non-empty day.
  local minutes_to_next = nil
  for _, transition in ipairs(today) do
    local trans_minutes = M.parse_hhmm(transition.time)
    if trans_minutes ~= nil and trans_minutes > minute then
      minutes_to_next = trans_minutes - minute
      break
    end
  end
  if minutes_to_next == nil then
    for fwd = 1, 7 do
      local list = M.day_list(days, (dow + fwd) % 7)
      if #list > 0 then
        local trans_minutes = M.parse_hhmm(list[1].time)
        if trans_minutes ~= nil then
          minutes_to_next = fwd * 1440 - minute + trans_minutes
          break
        end
      end
    end
  end

  return active, idx, minutes_to_next
end

-- validate checks a `days` table from the UI before it is persisted. Returns
-- true on success, or false plus an error message. Temperatures are bounded to
-- [lo, hi] (defaulting to a sane 5..35 heating range) so a typo can't drive a
-- radiator to 300° and so a value the climate device would reject is never
-- stored; callers pass the entity's min_temp/max_temp for the tighter bound.
function M.validate(days, lo, hi)
  lo = lo or 5
  hi = hi or 35
  if type(days) ~= "table" then
    return false, "days must be a table"
  end
  for dow = 0, 6 do
    local list = days[tostring(dow)]
    if list ~= nil then
      if type(list) ~= "table" then
        return false, "day " .. dow .. " must be a list"
      end
      for _, transition in ipairs(list) do
        if type(transition) ~= "table" then
          return false, "day " .. dow .. " has a non-table transition"
        end
        if M.parse_hhmm(transition.time) == nil then
          return false, "bad time: " .. tostring(transition.time)
        end
        if type(transition.temp) ~= "number" or transition.temp < lo or transition.temp > hi then
          return false, string.format("temp out of range (%g..%g): %s", lo, hi, tostring(transition.temp))
        end
      end
    end
  end
  return true
end

return M
