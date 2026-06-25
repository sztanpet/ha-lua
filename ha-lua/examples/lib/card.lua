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
-- `kind` is the published-entity prefix; it defaults to the script id. `data`
-- is passed through verbatim, so the helper mandates no field shape — only the
-- `script` routing (handled by ha.on_command) and the ha_lua_script marker it
-- stamps on every published sensor. Reusable by any future card-driven script.

local M = {}

-- new builds a card dispatcher. opts.kind sets the published-entity prefix
-- (defaults to ha.script_id). The returned table uses plain function fields
-- (dot calls, not methods) so callers write card.on / card.publish.
function M.new(opts)
	opts = opts or {}
	local kind = opts.kind or ha.script_id
	local handlers = {}

	local function entity_id(slug)
		return "sensor.ha_lua_" .. kind .. "_" .. slug
	end

	local card = {}

	-- on registers a handler for one command action. handler is called with the
	-- command's data payload. Returns card for chaining.
	function card.on(action, handler)
		handlers[action] = handler
		return card
	end

	-- publish creates/updates the companion sensor for slug, stamping the
	-- ha_lua_script marker so the entity is identifiable as ours. Returns the
	-- non-raising ha.set_state result (created:bool|nil, err).
	function card.publish(slug, state, attrs)
		attrs = attrs or {}
		attrs.ha_lua_script = ha.script_id
		return ha.set_state(entity_id(slug), state, attrs)
	end

	-- remove deletes the companion sensor for slug. Returns the non-raising
	-- ha.remove_state result (true|nil, err).
	function card.remove(slug)
		return ha.remove_state(entity_id(slug))
	end

	ha.on_command(function(action, data)
		local handler = handlers[action]
		if handler then
			handler(data)
		end
	end)

	return card
end

return M
