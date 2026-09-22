-- group_switches.lua
--
-- Wall switches driving one set of lamps: any flip of any switch in SWITCHES
-- forces every lamp in LAMPS to the same state — all on, or all off, never
-- half-lit. LAMPS may hold as many entities as you like, in any mix of
-- domains: a light, a relay in the switch domain, or a single Home Assistant
-- light group all behave the same here.
--
-- Direction always comes from the group's AGGREGATE state as it was just
-- before the press: if any lamp was on, everything goes off, otherwise
-- everything comes on. Never from the pressed switch's own state — several
-- switches drift apart, and so do the lamps when something outside this
-- script (the app, a schedule, a lost Zigbee command) moves one of them.
-- Following one switch instead of the room is what makes a press in a lit
-- room turn MORE lights on.
--
-- A switch may also BE one of the lamps, which is the normal case for a relay
-- whose own wall switch still drives it (no detached mode). Such a switch has
-- already flipped the lamp it feeds by the time the event reaches us, so it
-- counts as its OLD state in that aggregate — counted as its new state, every
-- press would agree with itself and the room would follow the one lamp that is
-- out of sync. The consequence is deliberate: press a relay whose lamp was off
-- while the rest of the room is lit and the room goes dark, its own lamp
-- included. A wall switch pressed in a lit room means "turn the room off".
--
-- Every switch is commanded along with the lamps, including one that is only an
-- input. Its own state still says nothing about whether the room is lit — that
-- is what LAMPS is for, and a pure input never votes — but leaving it wherever
-- the last flip left it means the relay, and the indicator on the wall,
-- disagrees with the room: switch the room on at one switch and off at another,
-- and the first one still reads "on".
--
-- A lamp may also change with no switch involved — the app, a schedule, a
-- voice assistant. FOLLOW_OUTSIDE_LAMP_CHANGE decides what that means: drag the
-- rest of the group along with it, or just note it so the next press still
-- reads the room correctly. Only lamps that are NOT also switches can be
-- treated that way; a report on a relay that is in SWITCHES may equally be its
-- own wall switch, and a wall switch is a toggle, not a level.
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

-- Whether a lamp that moves with no switch involved drags the rest of the group
-- with it: true means turning the LED on in the app switches the relays on with
-- it, false means the change is only noted (the group then only ever moves from
-- a switch). True keeps the promise this script exists for — the lamps are
-- never left half-lit, whatever moved one of them — so set it false if you have
-- an automation or schedule that is supposed to drive one lamp on its own.
local FOLLOW_OUTSIDE_LAMP_CHANGE = true

-- How long our own command outranks the lamps' reported state. Long enough to
-- cover a Zigbee round trip and a slow bulb, short enough that a lamp somebody
-- changed in the app is picked up by the next press.
local COMMAND_FRESH_SECS = 5

-- How long an unanswered command keeps expecting its echo. A command that
-- changed nothing (the lamp was already there, or it is offline) never
-- reports, so the expectation has to time out.
local ECHO_DEADLINE_SECS = 10

local is_switch = {}
for _, entity_id in ipairs(SWITCHES) do
  is_switch[entity_id] = true
end

local is_lamp = {}
for _, entity_id in ipairs(LAMPS) do
  is_lamp[entity_id] = true
end

-- Everything a press drives: the lamps, then the switches that are only inputs.
local COMMANDED = {}
for _, entity_id in ipairs(LAMPS) do
  table.insert(COMMANDED, entity_id)
end
for _, entity_id in ipairs(SWITCHES) do
  if not is_lamp[entity_id] then
    table.insert(COMMANDED, entity_id)
  end
end

-- FIFO of states we commanded and expect back. Everything we command is also
-- watched by one handler or the other, so everything can echo at us.
local expected_echoes = {}
for _, entity_id in ipairs(COMMANDED) do
  expected_echoes[entity_id] = {}
end

local last_command = nil

local function prune_expired(queue)
  local now = os.time()
  while queue[1] and queue[1].deadline < now do
    table.remove(queue, 1)
  end
end

-- The group's state as it stood just before `change` — a question about LAMPS
-- only, never about the switches driving them. The pressed entity counts as its
-- old state when it is itself a lamp; every other lamp counts as what it
-- currently reports, so a lamp moved outside this script is picked up by the
-- next press.
local function group_was_on(change)
  if last_command and os.time() - last_command.at < COMMAND_FRESH_SECS then
    return last_command.state == "on"
  end
  for _, lamp in ipairs(LAMPS) do
    local value
    if lamp == change.entity_id then
      value = change.old_state.state
    else
      local state = ha.get_state(lamp)
      value = state and state.state
    end
    if value == "on" then
      return true
    end
  end
  return false
end

local function switched(state)
  local value = state and state.state
  return value == "on" or value == "off"
end

-- True when this report is our own command coming back, which consumes the
-- expectation. Anything else clears the queue: once an entity has reported a
-- state we never asked for (a physical press racing our command, or a lost
-- echo) the expectations are meaningless.
local function is_our_echo(entity_id, new_state)
  local queue = expected_echoes[entity_id]
  if not queue then
    return false
  end
  prune_expired(queue)
  if not queue[1] then
    return false
  end
  if queue[1].state == new_state then
    table.remove(queue, 1)
    return true
  end
  expected_echoes[entity_id] = {}
  return false
end

-- Drive everything to `desired`. `source` is the entity whose report caused
-- this, and `source_state` the state it just reported.
local function force_group(desired, source, source_state)
  last_command = { state = desired, at = os.time() }
  for entity_id, pending in pairs(expected_echoes) do
    -- A source already in the state we are commanding will report nothing, and
    -- a phantom expectation would swallow the next real change on it. Everything
    -- else is mid-flip by construction.
    if not (entity_id == source and source_state == desired) then
      table.insert(pending, { state = desired, deadline = os.time() + ECHO_DEADLINE_SECS })
    end
  end

  ha.log("debug", "group_switches: " .. source .. " -> everything " .. desired)
  -- homeassistant.turn_on/off, not light.* or switch.*: it forwards each
  -- entity to its own domain, so one call covers a list that mixes a light
  -- with relays. Commanding every lamp (rather than toggling each) is
  -- what makes a half-lit room uniform again — a per-lamp toggle would only
  -- swap which lamp is on. wait = false so a second press is served without
  -- waiting out the round trip; failures reach ha.on_exception.
  ha.call_service("homeassistant", "turn_" .. desired, { entity_id = COMMANDED }, { wait = false })
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
    if is_our_echo(change.entity_id, new_state) then
      return
    end

    force_group(group_was_on(change) and "off" or "on", change.entity_id, new_state)
  end)
end

-- Lamps that are not switches. Nobody presses these: a report is either our own
-- command coming back, or the lamp moving on its own.
for _, lamp in ipairs(LAMPS) do
  if not is_switch[lamp] then
    ha.on_state_change(lamp, function(change)
      if not switched(change.old_state) or not switched(change.new_state) then
        return
      end
      local new_state = change.new_state.state
      if change.old_state.state == new_state then
        return
      end
      if is_our_echo(change.entity_id, new_state) then
        return
      end

      if FOLLOW_OUTSIDE_LAMP_CHANGE then
        force_group(new_state, change.entity_id, new_state)
        return
      end
      -- Not following it, but our remembered command has just become a lie:
      -- the room is not where we left it, and the next press must read the
      -- lamps rather than that command.
      last_command = nil
    end)
  end
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
