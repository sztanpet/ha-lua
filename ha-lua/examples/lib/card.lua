-- card.lua — ergonomic wrapper over ha.on_command + ha.set_state for scripts
-- driven by a Lovelace card.
--
-- A card fires one HA event, ha_lua_command, carrying {script, action, data}.
-- ha.on_command already routes by script id; this wrapper dispatches by action
-- and publishes companion sensors under a stable, derived entity id:
--
--   local card = require("card").new{ kind = "enhanced_climate" }
--   card.on("schedule", function(d) set_schedule(d.climate_entity, d.schedule) end)
--   card.publish(slug, state, attrs)   -- sensor.ha_lua_<kind>_<slug>
--   card.remove(slug)                  -- removes that sensor
--
-- `kind` is the published-entity prefix, defaulting to the script id. `data` is
-- passed through verbatim, so no field shape is mandated here — only the routing
-- and the ha_lua_script marker stamped on every published sensor.

local M = {}

-- Builds a card dispatcher. The returned table uses plain function fields (dot
-- calls, not methods), so callers write card.on / card.publish.
function M.new(opts)
  opts = opts or {}
  local kind = opts.kind or ha.script_id
  local handlers = {}

  local function entity_id(slug)
    return "sensor.ha_lua_" .. kind .. "_" .. slug
  end

  local card = {}

  -- Registers a handler for one action, called with the command's data payload.
  -- Returns card for chaining.
  function card.on(action, handler)
    handlers[action] = handler
    return card
  end

  -- Creates/updates the companion sensor for slug, stamped so the entity is
  -- identifiable as ours. Returns the non-raising ha.set_state result.
  function card.publish(slug, state, attrs)
    attrs = attrs or {}
    attrs.ha_lua_script = ha.script_id
    return ha.set_state(entity_id(slug), state, attrs)
  end

  -- Removes the companion sensor for slug, with ha.remove_state's result.
  function card.remove(slug)
    return ha.remove_state(entity_id(slug))
  end

  ha.on_command(function(action, data)
    local handler = handlers[action]
    if handler == nil then
      -- A card button that does nothing is otherwise indistinguishable from a
      -- broken one, from either side.
      ha.log("warn", kind .. ": no handler for card action " .. tostring(action))
      return
    end
    handler(data)
  end)

  return card
end

return M
