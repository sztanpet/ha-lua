-- group_switches.lua
--
-- One group of entities held in a single state — all on or all off, never half
-- of each. SWITCHES and LAMPS are two halves of that group: everything listed
-- is commanded together and counts towards the group's state, and being in
-- SWITCHES only adds that a change there is a press. A relay that feeds a lamp
-- belongs in both. Domains may be mixed freely.
--
-- A press inverts the group's aggregate state as it stood just before it, never
-- the pressed switch's own state: switches drift apart from each other and from
-- the lamps, and following one of them is what makes a press in a lit room turn
-- MORE lights on. The pressed entity counts as its OLD state there — its relay
-- has already flipped by the time the event arrives, and counting the new state
-- would make every press agree with itself. So pressing a relay that was off
-- while the rest of the group is on takes everything off, that relay included:
-- a wall switch pressed in a lit room means "turn the room off".
--
-- Two kinds of report are not presses. A device rejoining the Zigbee mesh
-- reports "unavailable" and then its state again, and Home Assistant sends a
-- state_changed for an attribute-only update; both would flip the room at 3am.
-- And a report on something we just commanded is our own echo — acted on as a
-- press it restarts the decision, which with the command still fresh inverts
-- the verdict and strobes the room. Each command therefore records the state it
-- expects back, and a matching report is consumed rather than acted on.
--
-- An entity that is not a switch can also move on its own, from the app or a
-- schedule; FOLLOW_OUTSIDE_CHANGE decides whether the group follows it.
--
-- Unlike the equivalent HA automation, the decision reads the group's state
-- through our own last command while that is fresh: a reported state lags the
-- command by the device round trip, so two presses inside that window would
-- both read "all off" and the second would be lost.
--
-- Edit SWITCHES and LAMPS to your entity ids (Developer Tools -> States).

-- A human is standing at the switch, so the default 100 ms batch window is
-- visible latency.
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

-- Whether an entity moved with no switch involved drags the group with it.
-- False leaves the group moving only from a switch — for a setup where a
-- schedule or automation is meant to drive one entity on its own.
local FOLLOW_OUTSIDE_CHANGE = true

-- How long our own command outranks the reported states: a Zigbee round trip
-- and a slow bulb, but short enough for the next press to see the real room.
local COMMAND_FRESH_SECS = 5

-- A command that changed nothing (already there, or offline) never reports, so
-- the expectation it left behind has to time out.
local ECHO_DEADLINE_SECS = 10

local is_switch = {}
for _, entity_id in ipairs(SWITCHES) do
  is_switch[entity_id] = true
end

-- Both lists are one group, deduplicated: a switch that feeds a lamp is in both.
local GROUP = {}
local in_group = {}
for _, list in ipairs({ LAMPS, SWITCHES }) do
  for _, entity_id in ipairs(list) do
    if not in_group[entity_id] then
      in_group[entity_id] = true
      GROUP[#GROUP + 1] = entity_id
    end
  end
end

-- Per entity, a FIFO of the states we commanded and expect it to report back.
local expected_echoes = {}
for _, entity_id in ipairs(GROUP) do
  expected_echoes[entity_id] = {}
end

local last_command = nil

local function prune_expired(queue)
  local now = os.time()
  while queue[1] and queue[1].deadline < now do
    table.remove(queue, 1)
  end
end

-- On if anything in the group was on just before `change`. The changed entity
-- counts as its old state; everything else as what it reports now, so anything
-- moved outside this script is picked up by the next press.
local function group_was_on(change)
  if last_command and os.time() - last_command.at < COMMAND_FRESH_SECS then
    return last_command.state == "on"
  end
  for _, entity_id in ipairs(GROUP) do
    local value
    if entity_id == change.entity_id then
      value = change.old_state.state
    else
      local state = ha.get_state(entity_id)
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

-- True when this report is our own command coming back, consuming the
-- expectation. Anything else clears the queue: a report we never asked for (a
-- press racing our command, a lost echo) makes the rest meaningless.
local function is_our_echo(entity_id, new_state)
  local queue = expected_echoes[entity_id]
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

-- Drive the whole group to `desired`. `source` is the entity whose report
-- caused this, `source_state` what it reported.
local function force_group(desired, source, source_state)
  last_command = { state = desired, at = os.time() }
  for entity_id, pending in pairs(expected_echoes) do
    -- A source already in the state being commanded reports nothing, and a
    -- phantom expectation would swallow the next real change on it.
    if not (entity_id == source and source_state == desired) then
      table.insert(pending, { state = desired, deadline = os.time() + ECHO_DEADLINE_SECS })
    end
  end

  ha.log("debug", "group_switches: " .. source .. " -> everything " .. desired)
  -- homeassistant.turn_on/off forwards each entity to its own domain, so one
  -- call covers mixed domains. Commanding a state rather than toggling each
  -- entity is what makes a half-lit group uniform again. wait = false so the
  -- next press is served without waiting out the round trip; failures still
  -- reach ha.on_exception.
  ha.call_service("homeassistant", "turn_" .. desired, { entity_id = GROUP }, { wait = false })
end

for _, entity_id in ipairs(GROUP) do
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

    if is_switch[change.entity_id] then
      force_group(group_was_on(change) and "off" or "on", change.entity_id, new_state)
    elseif FOLLOW_OUTSIDE_CHANGE then
      force_group(new_state, change.entity_id, new_state)
    else
      -- The remembered command has become a lie: the group is not where we
      -- left it, so the next press must read the entities instead.
      last_command = nil
    end
  end)
end

for _, entity_id in ipairs(GROUP) do
  if not ha.get_state(entity_id) then
    ha.log("warn", "group_switches: " .. entity_id ..
      " is unknown to the daemon — is that the right entity id?")
  end
end
