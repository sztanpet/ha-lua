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
function M.parse_hhmm(s)
  if type(s) ~= "string" then return nil end
  local h, m = s:match("^(%d%d):(%d%d)$")
  if h == nil then return nil end
  h, m = tonumber(h), tonumber(m)
  if h > 23 or m > 59 then return nil end
  return h * 60 + m
end

-- day_list returns a copy of the transitions for lua weekday dow (0=Mon..6=Sun)
-- sorted ascending by time. Malformed-time rows sort to the front but are
-- otherwise harmless; resolve() ignores them.
function M.day_list(days, dow)
  local raw = days and days[tostring(dow)]
  if type(raw) ~= "table" then return {} end
  local out = {}
  for _, t in ipairs(raw) do
    out[#out + 1] = t
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
  for i, t in ipairs(today) do
    local tm = M.parse_hhmm(t.time)
    if tm ~= nil and tm <= minute then
      active = t.temp
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
  for _, t in ipairs(today) do
    local tm = M.parse_hhmm(t.time)
    if tm ~= nil and tm > minute then
      minutes_to_next = tm - minute
      break
    end
  end
  if minutes_to_next == nil then
    for fwd = 1, 7 do
      local list = M.day_list(days, (dow + fwd) % 7)
      if #list > 0 then
        local tm = M.parse_hhmm(list[1].time)
        if tm ~= nil then
          minutes_to_next = fwd * 1440 - minute + tm
          break
        end
      end
    end
  end

  return active, idx, minutes_to_next
end

-- validate checks a `days` table from the UI before it is persisted. Returns
-- true on success, or false plus an error message. Temperatures are bounded to
-- a sane heating range so a typo can't drive a radiator to 300°.
function M.validate(days)
  if type(days) ~= "table" then
    return false, "days must be a table"
  end
  for dow = 0, 6 do
    local list = days[tostring(dow)]
    if list ~= nil then
      if type(list) ~= "table" then
        return false, "day " .. dow .. " must be a list"
      end
      for _, t in ipairs(list) do
        if type(t) ~= "table" then
          return false, "day " .. dow .. " has a non-table transition"
        end
        if M.parse_hhmm(t.time) == nil then
          return false, "bad time: " .. tostring(t.time)
        end
        if type(t.temp) ~= "number" or t.temp < 5 or t.temp > 35 then
          return false, "temp out of range (5..35): " .. tostring(t.temp)
        end
      end
    end
  end
  return true
end

return M
