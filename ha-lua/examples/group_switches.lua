-- group_switches.lua
--
-- Wall switches driving one set of lamps: any flip of any switch in SWITCHES
-- forces every lamp in LAMPS to the same state — all on, or all off, never
-- half-lit. LAMPS may hold as many entities as you like, in any mix of
-- domains: a light, a relay in the switch domain, or a single Home Assistant
-- light group all behave the same here.
--
-- Direction is a group toggle, not a copy of the switch's state: if any lamp
-- is on, everything goes off, otherwise everything comes on. Copying the
-- flipped switch's own state instead looks right with one switch and breaks
-- with several — their states drift apart, so pressing a switch that already
-- reads "on" while the lamps are on would do nothing at all, and a wall
-- switch that sometimes does nothing is indistinguishable from a broken one.
--
-- Only real on<->off transitions count as a press. A relay that drops off the
-- Zigbee mesh reports "unavailable" and reports its state again when it comes
-- back, and Home Assistant also sends a state_changed for an attribute-only
-- update; acting on either one flips the room in the middle of the night.
--
-- WHY THIS IS NOT AN HA AUTOMATION. The direction depends on the lamps'
-- current state, and a lamp's REPORTED state lags the command by the device
-- round trip (~200ms for Zigbee). Two presses inside that window both read
-- "all off" from the stale state and both turn everything on — the second
-- press is lost, which is exactly what makes the equivalent automation feel
-- broken when someone presses twice. So the decision is made against our own
-- last command while it is still fresh, and only then against what the lamps
-- report.
--
-- No echo attribution is needed (unlike mirrored_switches.lua) because
-- nothing here writes to the switches: the lamps we command are never
-- watched, so our own commands cannot come back as presses.
--
-- Edit SWITCHES and LAMPS to your entity ids (Developer Tools -> States).

-- A human is standing at the switch, so the default 100 ms batch window is
-- visible latency. See "ha.immediate_events" in lua_api.md.
ha.immediate_events()

local SWITCHES = {
  "switch.halo_ajtokapcsolo",
}

local LAMPS = {
  "light.bedroom_galeria_halo_led",
  "switch.galeria_lepcsokapcsolo",
}

-- How long our own command outranks the lamps' reported state. Long enough to
-- cover a Zigbee round trip and a slow bulb, short enough that a lamp somebody
-- changed in the app is picked up by the next press.
local COMMAND_FRESH_SECS = 5

local last_command = nil

local function group_is_on()
  if last_command and os.time() - last_command.at < COMMAND_FRESH_SECS then
    return last_command.state == "on"
  end
  for _, lamp in ipairs(LAMPS) do
    local state = ha.get_state(lamp)
    if state and state.state == "on" then
      return true
    end
  end
  return false
end

local function switched(state)
  local value = state and state.state
  return value == "on" or value == "off"
end

for _, entity_id in ipairs(SWITCHES) do
  ha.on_state_change(entity_id, function(change)
    if not switched(change.old_state) or not switched(change.new_state) then
      return
    end
    if change.old_state.state == change.new_state.state then
      return
    end

    local desired = group_is_on() and "off" or "on"
    last_command = { state = desired, at = os.time() }
    ha.log("debug", "group_switches: " .. change.entity_id .. " -> all lamps " .. desired)
    -- homeassistant.turn_on/off, not light.* or switch.*: it forwards each
    -- entity to its own domain, so one call covers a LAMPS list that mixes a
    -- light with a relay. Commanding every lamp (rather than toggling each) is
    -- what makes a half-lit room uniform again — a per-lamp toggle would only
    -- swap which lamp is on. wait = false so a second press is served without
    -- waiting out the round trip; failures reach ha.on_exception.
    ha.call_service("homeassistant", "turn_" .. desired, { entity_id = LAMPS }, { wait = false })
  end)
end

for _, list in ipairs({ SWITCHES, LAMPS }) do
  for _, entity_id in ipairs(list) do
    if not ha.get_state(entity_id) then
      ha.log("warn", "group_switches: " .. entity_id ..
        " is unknown to the daemon — is that the right entity id?")
    end
  end
end
