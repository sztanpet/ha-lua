-- galeria_stairs.lua
--
-- The hall wall switch also works the gallery staircase light: every flip of
-- switch.halo_ajtokapcsolo, in either direction, toggles
-- switch.galeria_lepcsokapcsolo. Software two-way staircase wiring — the two
-- relays are NOT mirrored, the hall one only delivers a pulse and the gallery
-- one keeps its own state.
--
-- Only real on<->off transitions count. A Zigbee relay that drops off the
-- network reports "unavailable" and then reports its state again when it
-- comes back, which is not a press: acting on it would flip the staircase
-- light in the middle of the night every time the mesh hiccups. Same for a
-- state_changed that only carries new attributes.
--
-- Edit SOURCE and TARGET to your entity ids (Developer Tools -> States).

-- A wall switch is exactly where the default 100 ms batch window is visible
-- to the person standing at it, and two relays produce no burst worth
-- batching. See "ha.immediate_events" in lua_api.md.
ha.immediate_events()

local SOURCE = "switch.halo_ajtokapcsolo"
local TARGET = "switch.galeria_lepcsokapcsolo"

local function switched(state)
  local value = state and state.state
  return value == "on" or value == "off"
end

ha.on_state_change(SOURCE, function(change)
  if not switched(change.old_state) or not switched(change.new_state) then
    return
  end
  if change.old_state.state == change.new_state.state then
    return
  end
  -- wait = false: don't park the event loop for the Zigbee round trip, a
  -- second press must be served immediately. Failures reach ha.on_exception.
  ha.call_service("switch", "toggle", { entity_id = TARGET }, { wait = false })
end)

for _, entity_id in ipairs({ SOURCE, TARGET }) do
  if not ha.get_state(entity_id) then
    ha.log("warn", "galeria_stairs: " .. entity_id ..
      " is unknown to the daemon — is that the right entity id?")
  end
end
