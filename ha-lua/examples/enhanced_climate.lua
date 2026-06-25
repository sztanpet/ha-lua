-- enhanced_climate.lua
--
-- A card-configured heating controller: each enhanced climate is one HA
-- `climate` entity wrapped with scheduling, timed overrides, manual-change
-- detection, and optional window cooperation — all provisioned at RUNTIME from
-- a Lovelace card (custom:ha-lua-enhanced-climate-card), never by editing this
-- file. See enhanced-climate-spec.md for the full design.
--
-- This is a NEW, standalone example, parallel to thermostat.lua (which keeps
-- its static lib/zones.lua model + Ingress editor). Both share the pure
-- lib/control.lua and lib/schedule.lua; only the wiring differs. Here the zone
-- definitions live in the card config, mirrored into the daemon over the
-- ha_lua_command event and surfaced back as companion sensor entities.
--
-- To use it: copy this file (and lib/) into /config/ha-lua/scripts/, add the
-- card to a dashboard, and point it at a climate entity.

local card = require("card").new { kind = "enhanced_climate" }

-- Registry: the set of enhanced climates, keyed by climate entity id, each
-- { climate_entity, window_sensors, presets }. Global-scoped (shared) under one
-- namespaced key so the whole set round-trips as a single value that the
-- control tick and the Ingress removal page can both iterate.
local REGISTRY_KEY = "enhanced_climate:registry"

local function load_registry()
  local reg = global.get(REGISTRY_KEY)
  if type(reg) ~= "table" then return {} end
  return reg
end

local function save_registry(reg)
  global.set(REGISTRY_KEY, reg)
end

-- normalize coerces a configure payload into the stored shape, defaulting the
-- optional lists so later code never has to type-check them.
local function normalize(data)
  return {
    climate_entity = data.climate_entity,
    window_sensors = type(data.window_sensors) == "table" and data.window_sensors or {},
    presets = type(data.presets) == "table" and data.presets or {},
  }
end

local function list_equal(a, b)
  if type(a) ~= "table" or type(b) ~= "table" then return a == b end
  if #a ~= #b then return false end
  for i = 1, #a do
    if a[i] ~= b[i] then return false end
  end
  return true
end

-- config_equal compares two stored configs so configure can no-op when nothing
-- changed (a card re-firing on every tab focus must not thrash the registry).
local function config_equal(x, y)
  if x == nil or y == nil then return x == y end
  return x.climate_entity == y.climate_entity
      and list_equal(x.window_sensors, y.window_sensors)
      and list_equal(x.presets, y.presets)
end

-- configure provisions (idempotent upsert) an enhanced climate. Fired by the
-- card on load / config change. Only mutates when the effective config actually
-- changed; control start + companion publish are wired in on top of this.
card.on("configure", function(data)
  if type(data) ~= "table" or type(data.climate_entity) ~= "string" or data.climate_entity == "" then
    return
  end
  local cfg = normalize(data)
  local reg = load_registry()
  if config_equal(reg[cfg.climate_entity], cfg) then return end
  reg[cfg.climate_entity] = cfg
  save_registry(reg)
end)

-- remove deprovisions an enhanced climate (from the Ingress removal page, §8).
-- Deleting the card does NOT fire this — removal is deliberately explicit.
card.on("remove", function(data)
  if type(data) ~= "table" or type(data.climate_entity) ~= "string" then return end
  local reg = load_registry()
  if reg[data.climate_entity] == nil then return end
  reg[data.climate_entity] = nil
  save_registry(reg)
end)

ha.on_exception(ha.exceptions.log_file("/config/ha-lua/logs/enhanced-climate-errors.log"))
