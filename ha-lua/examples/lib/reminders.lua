-- lib/reminders.lua
--
-- Durable reminders: deferred work that survives a restart, plus the
-- throttling every notification script ends up reinventing.
--
-- `ha.after` is only persisted when registered at load time, and the reminder
-- worth having — "warn me if the door is STILL open in ten minutes" — is armed
-- from inside a handler, so a restart in those ten minutes drops it. Pending work
-- lives in `store` instead, driven by one load-time `ha.every` tick: whatever
-- came due while the daemon was down fires on the next tick after boot.
--
-- A callback cannot be stored in SQLite, so actions are named. Register them
-- at load time with define(), then schedule() them by name from anywhere.
--
--   local reminders = require "reminders"
--
--   reminders.define("door_still_open", function(payload)
--     ha.call_service("notify", "persistent_notification",
--       { message = payload.message })
--   end)
--
--   ha.on_state_change("binary_sensor.front_door", function(data)
--     if data.new_state.state == "on" then
--       reminders.schedule("front_door", "door_still_open", "10m",
--         { message = "Front door has been open for 10 minutes" })
--     else
--       reminders.cancel("front_door")
--     end
--   end)
--
--   reminders.start()   -- last, after every define()
--
-- Keys are yours to choose and are the unit of cancellation: scheduling the
-- same key twice replaces the pending reminder rather than stacking a second
-- one, which is what makes the door-closed path a plain cancel().

local M = {}

-- One key holds the whole pending table: a handful of reminders is the realistic
-- load, and one row keeps the fire path a single read.
local PENDING_KEY = "reminders:pending"
local THROTTLE_KEY = "reminders:throttle"

local actions = {}
local started = false

-- Raises on nonsense at the call site, rather than silently never firing.
local function seconds(spec)
  if type(spec) == "number" then return spec end
  return time.parse_duration(spec)
end

local function now()
  return time.now():unix()
end

local function load_pending()
  return store.get(PENDING_KEY) or {}
end

local function save_pending(pending)
  store.set(PENDING_KEY, pending)
end

-- Registers a named action. Call at load time, before start(): a reminder that
-- comes due with no action defined is logged and dropped.
function M.define(name, fn)
  if type(name) ~= "string" or name == "" then
    error("reminders.define: name must be a non-empty string", 2)
  end
  if type(fn) ~= "function" then
    error("reminders.define: " .. name .. " needs a function", 2)
  end
  actions[name] = fn
end

-- Sets (or replaces) the reminder under `key`. Safe from inside a callback, and
-- across a restart.
function M.schedule(key, action, delay, payload)
  if type(key) ~= "string" or key == "" then
    error("reminders.schedule: key must be a non-empty string", 2)
  end
  local pending = load_pending()
  pending[key] = {
    action = action,
    due = now() + seconds(delay),
    payload = payload,
  }
  save_pending(pending)
end

-- A reminder that repeats on a widening ladder: each delay in `steps` in turn,
-- stopping after the last. The action receives payload plus `step` (1-based) and
-- `final`, so it can say "still open" first and shout last. cancel() ends it.
function M.escalate(key, action, steps, payload)
  if type(steps) ~= "table" or #steps == 0 then
    error("reminders.escalate: steps must be a non-empty list of delays", 2)
  end
  local pending = load_pending()
  pending[key] = {
    action = action,
    due = now() + seconds(steps[1]),
    payload = payload,
    steps = steps,
    step = 1,
  }
  save_pending(pending)
end

-- Drops a pending reminder or ladder. Cancelling a key that is not pending is
-- not an error: the caller is a state handler that cannot know if it armed one.
function M.cancel(key)
  local pending = load_pending()
  if pending[key] == nil then return false end
  pending[key] = nil
  save_pending(pending)
  return true
end

-- due_at returns the Unix time the reminder under `key` will fire, or nil.
function M.due_at(key)
  local entry = load_pending()[key]
  return entry and entry.due or nil
end

-- The "do not spam me" gate: true at most once per `window`.
--
--   if reminders.throttle("low_battery", "6h") then ...notify... end
--
-- The last-fired times live in the store too, so a restart loop cannot turn one
-- notification an hour into one per boot.
function M.throttle(key, window)
  local last = store.get(THROTTLE_KEY) or {}
  local moment = now()
  if last[key] ~= nil and moment - last[key] < seconds(window) then
    return false
  end
  last[key] = moment
  store.set(THROTTLE_KEY, last)
  return true
end

-- Clears a throttle. Use it when the condition clears, so a recurrence notifies
-- at once instead of waiting out a window that is no longer about anything.
function M.forget(key)
  local last = store.get(THROTTLE_KEY) or {}
  if last[key] == nil then return false end
  last[key] = nil
  store.set(THROTTLE_KEY, last)
  return true
end

local function fire(key, entry, step, final)
  local fn = actions[entry.action]
  if fn == nil then
    ha.log("warn", "reminders: no action named " .. tostring(entry.action) ..
      " for key " .. key .. " (define() it at load time)")
    return
  end
  local payload = entry.payload or {}
  payload.step = step
  payload.final = final
  fn(payload)
end

-- Fires everything due and re-arms ladders. Exposed so a test, or a script
-- wanting a different cadence, can drive it directly.
function M.tick()
  local pending = load_pending()
  local moment = now()
  local due = {}

  for key, entry in pairs(pending) do
    if entry.due <= moment then
      local step = entry.step or 1
      local final = entry.steps == nil or step >= #entry.steps
      if final then
        pending[key] = nil
      else
        entry.step = step + 1
        entry.due = moment + seconds(entry.steps[entry.step])
      end
      due[#due + 1] = { key = key, entry = entry, step = step, final = final }
    end
  end
  if #due == 0 then return end

  -- Persist the re-arm before running anything: an action that raises leaves
  -- the store already advanced, so it fires once rather than every tick.
  save_pending(pending)
  for _, item in ipairs(due) do
    fire(item.key, item.entry, item.step, item.final)
  end
end

-- Installs the tick. Call once at load time, after every define(). `opts.tick`
-- (default 30s) is also a reminder's worst-case lateness.
--
-- The first tick runs immediately: after a restart, the reminders that came due
-- while the daemon was down are the ones somebody is waiting on.
function M.start(opts)
  if started then return end
  started = true
  local cadence = (opts and opts.tick) or "30s"
  ha.every(cadence, M.tick)
  M.tick()
end

return M
