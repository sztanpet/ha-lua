-- mirrored_switches.lua
--
-- Keeps two switches mirrored: when either one turns on or off, the other
-- follows. The classic use case is two Zigbee relays wired to the same
-- hallway lights — flipping either wall switch must light the whole hallway.
--
-- Replaces the usual HA automation pyramid (two device triggers, a trigger-id
-- branch, then an is_on branch each side) with one handler per switch that
-- copies its new state to the partner.
--
-- The hard part is telling a real press on the partner apart from the echo
-- of our own command — the partner's state_changed fires either way. The
-- naive fix ("skip if the partner's state already matches") breaks under
-- fast toggling: the partner's REPORTED state lags its COMMANDED state by
-- the device round trip (~200ms for Zigbee), so a second flip inside that
-- window sees the stale state, wrongly skips its command, and the toggle is
-- lost — worse, the late echo then flips the pressed switch back.
--
-- So this script attributes echoes instead of comparing states: every
-- command records the state it expects the partner to report, a report that
-- matches the oldest expectation is consumed as our echo, and new presses
-- compare against the partner's *commanded* state (newest expectation) with
-- its reported state as the fallback. Expectations expire after a few
-- seconds so a command that produced no report (device already there, or
-- offline) can't poison the attribution forever.
--
-- Only real "on"/"off" transitions are acted on; "unavailable"/"unknown"
-- (e.g. a device dropping off the Zigbee network) are ignored, so a
-- flapping device never toggles its partner.
--
-- Edit SWITCHES to your two entity ids (Developer Tools -> States).

-- By default the daemon batches events for 100 ms before running handlers
-- (see "ha.immediate_events" in lua_api.md). Batching is the default because
-- unbatched bursts overflowed script event channels and dropped events
-- outright. Two switches produce no such bursts, and a wall switch is
-- exactly the case where the delay is human-visible: the partner light
-- would lag a tenth of a second behind the pressed one. Opt into immediate
-- delivery so the mirror reacts as fast as a built-in HA automation.
ha.immediate_events()

local SWITCHES = {
  "switch.zbminir2_bejaratiajtokapcsolo",
  "switch.zbminir2_folyoso",
}

-- Per-entity FIFO of states we commanded and expect the device to report
-- back. Oldest first; each entry carries a deadline past which it is stale.
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

-- The state a switch is headed for: the newest outstanding command if one is
-- in flight, else the state it last reported. Comparing presses against this
-- (never the raw report) is what keeps fast toggles from being lost.
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
    local new_state = change.new_state.state
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
    -- wait = false: don't park this script's event loop until Home Assistant
    -- confirms the call (that includes the Zigbee round trip). Without it, a
    -- quick on-then-off delivers the second flip only after the first one's
    -- confirmation. Failures still surface via ha.on_exception.
    ha.call_service("switch", "turn_" .. new_state, { entity_id = partner }, { wait = false })
  end)
end
