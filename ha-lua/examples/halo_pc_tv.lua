-- halo_pc_tv.lua
--
-- The bedroom TV follows the bedroom PC: on when the PC comes online, off when
-- it goes offline.
--
-- Online is a TCP connect to the PC's SMB port every couple of seconds. HA's
-- Ping integration polls every 30 s, and its binary sensor drops on a single
-- round with no reply. One answered probe is online; OFF_AFTER_MISSES in a row
-- is offline. The first verdict after a load is taken as is, so a restart or
-- reload neither wakes nor kills the TV.
--
-- The TV is retried until it REPORTS the wanted state: a TV in deep standby
-- drops its remote connection and the call raises, and one that is busy can
-- take the call and ignore it.

local PC = "192.168.1.12:445"
local TV = "media_player.bedroom_tv"
local PROBE_EVERY = "2s"
-- The PC answers in under a millisecond on the wired LAN; a dropped SYN is
-- only resent after 1 s, so waiting longer than this buys nothing.
local PROBE_TIMEOUT = "250ms"
-- A busy PC can miss one probe; only a run of them is a shutdown.
local OFF_AFTER_MISSES = 3
-- Both directions send the same POWER key, so a retry before the TV has
-- reported the last one would toggle it straight back.
local RETRY_EVERY = "20s"
local ATTEMPTS = 5

-- Bumped on every PC edge; a retry chain stops once it is no longer current.
local generation = 0
local online -- nil until the first verdict
local misses = 0

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

ha.every(PROBE_EVERY, function()
  local verdict
  if net.reachable(PC, PROBE_TIMEOUT) then
    misses = 0
    verdict = true
  else
    misses = misses + 1
    if misses < OFF_AFTER_MISSES then return end
    verdict = false
  end
  if verdict == online then return end
  local first = online == nil
  online = verdict
  if first then return end
  generation = generation + 1
  drive(online and "on" or "off", generation, 1)
end)

if not ha.get_state(TV) then
  ha.log("warn", TV .. " is unknown to the daemon — is that the right entity id?")
end

ha.on_exception(ha.exceptions.log_file("halo_pc_tv-errors.log"))
