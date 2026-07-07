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
--   * The "partner already matches" guard is what stops the echo loop: when
--     switch A syncs switch B, B's own state change fires the handler again,
--     sees A already matches, and stops.
--   * Only real "on"/"off" transitions are acted on; "unavailable"/"unknown"
--     (e.g. a device dropping off the Zigbee network) are ignored, so a
--     flapping device never toggles its partner.
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

local function partner_of(entity_id)
  if entity_id == SWITCHES[1] then
    return SWITCHES[2]
  end
  return SWITCHES[1]
end

for _, entity_id in ipairs(SWITCHES) do
  ha.on_state_change(entity_id, function(change)
    local new_state = change.new_state.state
    if new_state ~= "on" and new_state ~= "off" then
      return
    end

    local partner = partner_of(change.entity_id)
    local partner_state = ha.get_state(partner)
    if partner_state and partner_state.state == new_state then
      return
    end

    ha.call_service("switch", "turn_" .. new_state, { entity_id = partner })
  end)
end
