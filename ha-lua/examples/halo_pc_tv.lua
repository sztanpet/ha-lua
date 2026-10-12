-- halo_pc_tv.lua
--
-- The bedroom TV follows the bedroom PC: on when the PC comes online, off when
-- it goes offline.
--
-- Online comes from HA's Ping integration on 192.168.1.12, whose consider_home
-- already rides out a lost ping round. Only clean on/off edges count, so a
-- sensor going unavailable and back (an HA restart) neither wakes nor kills
-- the TV.
--
-- The TV is retried until it REPORTS the wanted state: a TV in deep standby
-- drops its remote connection and the call raises, and one that is busy can
-- take the call and ignore it.

local PC = "binary_sensor.bedroom_halo_pc"
local TV = "media_player.bedroom_tv"
-- Both directions send the same POWER key, so a retry before the TV has
-- reported the last one would toggle it straight back.
local RETRY_EVERY = "20s"
local ATTEMPTS = 5

-- Bumped on every PC edge; a retry chain stops once it is no longer current.
local generation = 0

local function drive(want, mine, attempt)
  if mine ~= generation then return end
  local tv = ha.get_state(TV)
  local current = tv and tv.state or "missing"
  if current == want then return end
  if attempt > ATTEMPTS then
    ha.log("warn", string.format("halo_pc_tv: %s still %s after %d tries to turn it %s",
      TV, current, ATTEMPTS, want))
    return
  end
  local ok, err = pcall(ha.call_service, "media_player", "turn_" .. want, { entity_id = TV })
  if not ok then
    ha.log("info", string.format("halo_pc_tv: turn_%s try %d failed: %s", want, attempt, tostring(err)))
  end
  ha.after(RETRY_EVERY, function() drive(want, mine, attempt + 1) end)
end

ha.on_state_change(PC, function(data)
  local old = data.old_state and data.old_state.state
  local new = data.new_state and data.new_state.state
  if (old == "off" and new == "on") or (old == "on" and new == "off") then
    generation = generation + 1
    drive(new, generation, 1)
  end
end)

for _, entity_id in ipairs({ PC, TV }) do
  if not ha.get_state(entity_id) then
    ha.log("warn", entity_id .. " is unknown to the daemon — is that the right entity id?")
  end
end

ha.on_exception(ha.exceptions.log_file("halo_pc_tv-errors.log"))
