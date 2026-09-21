-- group_switches.lua
--
-- Wall switches driving one set of lamps: any flip of any switch in SWITCHES
-- forces every lamp in LAMPS to the same state — all on, or all off, never
-- half-lit. LAMPS may hold as many entities as you like, in any mix of
-- domains: a light, a relay in the switch domain, or a single Home Assistant
-- light group all behave the same here.
--
-- A switch may also BE one of the lamps, which is the normal case for a relay
-- whose own wall switch still drives it (no detached mode). Those two roles
-- decide the direction between them:
--
--   * A switch that is NOT a lamp is a pure input — its on/off state means
--     nothing by itself, so a flip is a group toggle: if any lamp is on,
--     everything goes off, otherwise everything comes on. Copying such a
--     switch's state instead looks right with one switch and breaks with
--     several, because their states drift apart: pressing one that already
--     reads "on" while the lamps are on would do nothing at all, and a wall
--     switch that sometimes does nothing is indistinguishable from a broken
--     one.
--   * A switch that IS a lamp has already changed the light it controls, so
--     its new state is the intent and the other lamps are forced to match it.
--     Toggling the group here would fight the person, taking the lamp they
--     just switched on straight back off.
--
-- Only real on<->off transitions count as a press. A relay that drops off the
-- Zigbee mesh reports "unavailable" and reports its state again when it comes
-- back, and Home Assistant also sends a state_changed for an attribute-only
-- update; acting on either one flips the room in the middle of the night.
--
-- WHY THIS IS NOT AN HA AUTOMATION. Two reasons, both about state that lags:
--
-- A lamp's REPORTED state lags the command by the device round trip (~200ms
-- for Zigbee). Two presses inside that window both read "all off" from the
-- stale state and both turn everything on — the second press is lost, which
-- is what makes the equivalent automation feel broken when someone presses
-- twice. So the group's state comes from our own last command while that is
-- still fresh, and only then from what the lamps report.
--
-- And a lamp we command that is also a switch we watch reports our own
-- command back at us. Mistaken for a press, that report restarts the
-- decision — and with our command fresh, the verdict flips, so the room
-- strobes until the deadline. So every command records the state it expects
-- each watched lamp to report, oldest first, and a matching report is
-- consumed as our echo instead of acted on (the mirrored_switches.lua
-- lesson). Expectations expire, so a command that produced no report cannot
-- poison the attribution forever.
--
-- Edit SWITCHES and LAMPS to your entity ids (Developer Tools -> States).

-- A human is standing at the switch, so the default 100 ms batch window is
-- visible latency. See "ha.immediate_events" in lua_api.md.
ha.immediate_events()

local SWITCHES = {
  "switch.halo_ajtokapcsolo",
  "switch.halo_ajtoszekrenykapcsolo",
  "switch.galeria_lepcsokapcsolo",
}

local LAMPS = {
  "light.bedroom_galeria_halo_led",
  "switch.galeria_lepcsokapcsolo",
  "switch.halo_ajtoszekrenykapcsolo",
}

-- How long our own command outranks the lamps' reported state. Long enough to
-- cover a Zigbee round trip and a slow bulb, short enough that a lamp somebody
-- changed in the app is picked up by the next press.
local COMMAND_FRESH_SECS = 5

-- How long an unanswered command keeps expecting its echo. A command that
-- changed nothing (the lamp was already there, or it is offline) never
-- reports, so the expectation has to time out.
local ECHO_DEADLINE_SECS = 10

local is_lamp = {}
for _, entity_id in ipairs(LAMPS) do
  is_lamp[entity_id] = true
end

-- FIFO of states we commanded and expect back, per entity that is both
-- watched and commanded. Nothing else can echo at us.
local expected_echoes = {}
for _, entity_id in ipairs(SWITCHES) do
  if is_lamp[entity_id] then
    expected_echoes[entity_id] = {}
  end
end

local last_command = nil

local function prune_expired(queue)
  local now = os.time()
  while queue[1] and queue[1].deadline < now do
    table.remove(queue, 1)
  end
end

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
    local new_state = change.new_state.state
    if change.old_state.state == new_state then
      return
    end

    local queue = expected_echoes[change.entity_id]
    if queue then
      prune_expired(queue)
      if queue[1] then
        if queue[1].state == new_state then
          table.remove(queue, 1)
          return
        end
        -- The device reported something we never commanded (a physical press
        -- racing our command, or a lost echo). The expectations are
        -- meaningless now — drop them and treat this as a real press.
        expected_echoes[change.entity_id] = {}
      end
    end

    local desired = new_state
    if not is_lamp[change.entity_id] then
      desired = group_is_on() and "off" or "on"
    end

    last_command = { state = desired, at = os.time() }
    for watched, pending in pairs(expected_echoes) do
      -- The pressed lamp is already in the state we are about to command, so
      -- it will report nothing; a phantom expectation would swallow the next
      -- real press. Every other watched lamp is mid-flip by construction.
      if not (watched == change.entity_id and new_state == desired) then
        table.insert(pending, { state = desired, deadline = os.time() + ECHO_DEADLINE_SECS })
      end
    end

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

local warned = {}
for _, list in ipairs({ SWITCHES, LAMPS }) do
  for _, entity_id in ipairs(list) do
    if not warned[entity_id] and not ha.get_state(entity_id) then
      warned[entity_id] = true
      ha.log("warn", "group_switches: " .. entity_id ..
        " is unknown to the daemon — is that the right entity id?")
    end
  end
end
