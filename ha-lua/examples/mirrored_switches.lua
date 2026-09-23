-- mirrored_switches.lua
--
-- Keeps two switches mirrored: when either turns on or off, the other follows.
-- The case is two relays wired to the same hallway lights, where flipping
-- either wall switch must light the whole hallway.
--
-- The hard part is telling a real press on the partner from the echo of our own
-- command, since the partner reports either way. Comparing states does not do
-- it: a partner's reported state lags its commanded state by the device round
-- trip (~200ms for Zigbee), so a second flip inside that window sees the stale
-- state, wrongly skips its command, and the toggle is lost — then the late echo
-- flips the pressed switch back.
--
-- So echoes are attributed instead: every command records the state it expects
-- the partner to report, a report matching the oldest expectation is consumed as
-- our echo, and a new press compares against the partner's COMMANDED state.
-- Expectations expire, so a command that produced no report (already there, or
-- offline) cannot poison the attribution forever.
--
-- Only "on"/"off" transitions are acted on; "unavailable"/"unknown" from a
-- device dropping off the mesh must never toggle its partner.
--
-- Edit SWITCHES to your two entity ids (Developer Tools -> States).

-- A wall switch is where the default 100 ms batch window is human-visible: the
-- partner would lag a tenth of a second behind the pressed one.
ha.immediate_events()

local SWITCHES = {
  "switch.zbminir2_bejaratiajtokapcsolo",
  "switch.zbminir2_folyoso",
}

-- Per-entity FIFO of states we commanded and expect back, with a deadline
-- past which an unanswered one is stale.
local expected_echoes = {
  [SWITCHES[1]] = {},
  [SWITCHES[2]] = {},
}
local ECHO_DEADLINE_SECS = 10

local function prune_expired(queue)
  local now = os.time()
  while queue[1] and queue[1].deadline < now do
    table.remove(queue, 1)
  end
end

local function partner_of(entity_id)
  if entity_id == SWITCHES[1] then
    return SWITCHES[2]
  end
  return SWITCHES[1]
end

-- The state a switch is headed for: the newest outstanding command, else what
-- it last reported. Comparing presses against this is what keeps a fast toggle
-- from being lost.
local function commanded_state(entity_id)
  local queue = expected_echoes[entity_id]
  prune_expired(queue)
  if #queue > 0 then
    return queue[#queue].state
  end
  local current = ha.get_state(entity_id)
  return current and current.state or nil
end

for _, entity_id in ipairs(SWITCHES) do
  ha.on_state_change(entity_id, function(change)
    local new_state = change.new_state and change.new_state.state
    if new_state ~= "on" and new_state ~= "off" then
      return
    end

    local queue = expected_echoes[change.entity_id]
    prune_expired(queue)
    if queue[1] then
      if queue[1].state == new_state then
        -- Our own command reporting back; consume it, don't bounce it.
        table.remove(queue, 1)
        return
      end
      -- The device reported something we didn't command (a physical press
      -- racing our command, or a lost echo). The expectations are now
      -- meaningless — drop them and treat this as a real press.
      expected_echoes[change.entity_id] = {}
    end

    local partner = partner_of(change.entity_id)
    if commanded_state(partner) == new_state then
      return
    end

    table.insert(expected_echoes[partner], {
      state = new_state,
      deadline = os.time() + ECHO_DEADLINE_SECS,
    })
    -- wait = false: parking on HA's confirmation (which includes the Zigbee
    -- round trip) would delay the next flip by it. Failures still surface via
    -- ha.on_exception.
    ha.call_service("switch", "turn_" .. new_state, { entity_id = partner }, { wait = false })
  end)
end
