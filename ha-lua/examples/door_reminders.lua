-- door_reminders.lua
--
-- Tell me the door is still open, keep telling me, and stop when I close it. An
-- HA automation parked in a `delay:` is killed by a restart, so the reminder is
-- lost exactly when it matters — the door has been open the whole time the box
-- was rebooting. lib/reminders.lua keeps the pending work in SQLite and drives
-- it from one load-time tick instead.
--
-- It also shows the two pieces that make a nag defensible rather than annoying:
-- a throttle, so a flaky sensor cannot notify twenty times an hour, and
-- ha.duration_in_state, so the message says how long the door has actually been
-- open rather than asserting the ladder's own number.
--
-- Edit DOORS and NOTIFY_TARGET for your setup.

local reminders = require "reminders"

-- Where to send the alert: a HA notify service written "<domain>.<service>".
local NOTIFY_TARGET = "notify.pixel_9a"

-- The doors to watch, and what to call them in the message.
local DOORS = {
  ["binary_sensor.front_door"] = "Front door",
  ["binary_sensor.back_door"]  = "Back door",
}

-- How long the door may stand open before the first nag, then the gaps
-- between the reminders after it. The ladder ends after the last step.
local LADDER = { "10m", "20m", "1h" }

-- One nag per door per window, whatever the sensor does: a door that chatters
-- is a sensor problem, and notifying thirty times does not fix the sensor.
local NAG_THROTTLE = "5m"

local function notify(message)
  local domain, service = NOTIFY_TARGET:match("^(.-)%.(.+)$")
  if domain == nil then
    ha.log("error", "NOTIFY_TARGET must be '<domain>.<service>': " .. NOTIFY_TARGET)
    return
  end
  ha.call_service(domain, service, { message = message })
end

reminders.define("door_open", function(payload)
  local name = payload.name or "A door"
  if not reminders.throttle("door_nag:" .. payload.entity_id, NAG_THROTTLE) then
    return
  end

  -- The second return says whether history reaches back far enough; when it
  -- does not, say nothing about the duration rather than something wrong.
  local open_for, complete = ha.duration_in_state(
    payload.entity_id, "on", time.now():add(-24 * time.hour))

  local message = name .. " is still open"
  if complete and open_for >= 60 then
    message = message .. " (" .. math.floor(open_for / 60) .. " minutes)"
  end
  if payload.final then
    message = message .. " — last reminder"
  end
  notify(message)
end)

for entity_id, name in pairs(DOORS) do
  ha.on_state_change(entity_id, function(data)
    local old_state = data.old_state and data.old_state.state
    local new_state = data.new_state and data.new_state.state
    -- Only the two real transitions: an attribute-only update would restart the
    -- ladder at step one on every report, so the first nag would never come due,
    -- and "unavailable" would cancel a nag for a door that is still open.
    if new_state == "on" and old_state ~= "on" then
      reminders.escalate(entity_id, "door_open", LADDER, {
        entity_id = entity_id,
        name = name,
      })
    elseif new_state == "off" then
      -- Reopen the throttle too, so the next opening notifies on its own merits
      -- instead of waiting out a stale window.
      reminders.cancel(entity_id)
      reminders.forget("door_nag:" .. entity_id)
    end
  end)
end

-- After every define(): installs the tick and fires whatever came due while the
-- daemon was down.
reminders.start()

ha.on_exception(ha.exceptions.log_file("door-reminders.log"))
