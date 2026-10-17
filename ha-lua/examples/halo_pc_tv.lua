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
-- The probe only answers once Windows is well into resuming, ~30 s after the
-- power button. The PC hibernates; its NIC holds a wake-on-LAN link then and
-- renegotiates at power-on, dropping the router port for about a second. HA's
-- MikroTik integration polls every 30 s and misses that, so the script reads
-- the port's link-downs counter from the router's REST API itself. A drop
-- while the PC has been gone a while turns the TV on early; one not followed
-- by the PC answering turns it back off. The router login, "user:password",
-- is read from global.get("mikrotik_login"); without it only the probe runs.
--
-- The TV is retried until it REPORTS the wanted state: a TV in deep standby
-- drops its remote connection and the call raises, and one that is busy can
-- take the call and ignore it.

local PC = "192.168.1.12:445"
local TV = "media_player.bedroom_tv"
local PROBE_EVERY = "2s"
-- The PC answers in under a millisecond on the wired LAN; a dropped SYN is
-- only resent after 1 s, so waiting longer buys nothing. While the PC is off
-- every probe runs to the deadline, and the script's events wait behind it.
local PROBE_TIMEOUT = "100ms"
-- A busy PC can miss one probe; only a run of them is it going down.
local OFF_AFTER_MISSES = 3
-- Both directions send the same POWER key, so a retry before the TV has
-- reported the last one would toggle it straight back.
local RETRY_EVERY = "20s"
local ATTEMPTS = 5
local ROUTER = "http://192.168.1.1"
local PORT = "ether3"
local LINK_EVERY = "1s"
-- The link also drops as the PC hibernates or reboots. Hibernate writes RAM to
-- disk after 445 closes and only then cuts the link, so this must outlast that.
local LINK_AFTER_OFFLINE = time.parse_duration("60s")
local BOOT_GRACE = "2m"

-- Bumped on every PC edge; a retry chain stops once it is no longer current.
local generation = 0
local online -- nil until the first verdict
local misses = 0
local offline_since = time.now()
local link_downs -- the router's counter at the last read
local router_error -- logged once per outage

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
  if not online then offline_since = time.now() end
  if first then return end
  generation = generation + 1
  drive(online and "on" or "off", generation, 1)
end)

local function link_dropped()
  if online ~= false then return end
  if time.now():sub(offline_since) < LINK_AFTER_OFFLINE then return end
  generation = generation + 1
  local mine = generation
  drive("on", mine, 1)
  ha.after(BOOT_GRACE, function()
    if mine ~= generation or online then return end
    ha.log("info", "halo_pc_tv: " .. PORT .. " dropped but the PC never answered")
    generation = generation + 1
    drive("off", generation, 1)
  end)
end

local function read_link_downs(auth)
  local res, err = http.get(ROUTER .. "/rest/interface/" .. PORT .. "?.proplist=link-downs",
    { Authorization = auth })
  if not res then return nil, err end
  if res.status ~= 200 then return nil, "HTTP " .. res.status end
  local ok, port = pcall(json.decode, res.body)
  local count = ok and type(port) == "table" and tonumber(port["link-downs"])
  if not count then return nil, "unexpected reply " .. res.body end
  return count
end

local login = global.get("mikrotik_login")
if login then
  local auth = "Basic " .. crypto.base64_encode(login)
  ha.every(LINK_EVERY, function()
    local count, err = read_link_downs(auth)
    if not count then
      if not router_error then
        ha.log("warn", "halo_pc_tv: cannot read " .. PORT .. " from " .. ROUTER .. ": " .. tostring(err))
      end
      router_error = true
      return
    end
    router_error = nil
    local before = link_downs
    link_downs = count
    -- A router reboot resets the counter; that is a new baseline, not a drop.
    if before and count > before then link_dropped() end
  end)
else
  ha.log("warn", "halo_pc_tv: global mikrotik_login is not set; waking on the probe alone")
end

if not ha.get_state(TV) then
  ha.log("warn", TV .. " is unknown to the daemon — is that the right entity id?")
end

ha.on_exception(ha.exceptions.log_file("halo_pc_tv-errors.log"))
