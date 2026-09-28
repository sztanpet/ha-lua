-- enhanced_climate.lua
--
-- A card-configured heating controller: each enhanced climate is one HA `climate`
-- entity wrapped with scheduling, timed overrides, manual-change detection and
-- optional window cooperation, all provisioned at RUNTIME from a Lovelace card
-- (custom:ha-lua-enhanced-climate-card) rather than by editing this file. See
-- enhanced-climate-spec.md for the design.
--
-- It runs parallel to thermostat.lua, which keeps the static lib/zones.lua model
-- and an Ingress editor; both share lib/control.lua and lib/schedule.lua and
-- differ only in wiring. Here the definitions live in the card config, mirrored
-- into the daemon over the ha_lua_command event and surfaced back as companion
-- sensors.
--
-- To use it: copy this file and lib/ into /config/ha-lua/scripts/, add the card
-- to a dashboard, and point it at a climate entity.

local control = require "control"
local schedule = require "schedule"
local climate_lib = require "climate"
local overshoot = require "overshoot"
local card = require("card").new { kind = "enhanced_climate" }

-- Seed value (°C) for an enhanced climate's override temperature — the setpoint
-- a timed override drives it to — until the user edits it via the card.
local DEFAULT_OVERRIDE_TEMP = 23

-- Setpoint held while any bound window is open, clamped to the device's own min.
-- Writing a low setpoint is how a self-contained controller PAUSES heating:
-- skipping the write would leave the device coasting at its last target.
local FROST_TEMP = 15

-- Per climate entity, the last companion payload written and when, so an
-- unchanged sensor is not rewritten every minute. PUBLISH_HEARTBEAT bounds how
-- long it goes unrefreshed, so it still self-heals after an HA restart drops it.
local published = {}
local PUBLISH_HEARTBEAT = 5 * 60

-- ---------------------------------------------------------------------------
-- Registry (§7.1): the enhanced climates, keyed by climate entity id. Global and
-- under one key, so the whole set round-trips as a single value that both the
-- control tick and the Ingress removal page iterate.
-- ---------------------------------------------------------------------------

local REGISTRY_KEY = "enhanced_climate:registry"

local function load_registry()
  local reg = global.get(REGISTRY_KEY)
  if type(reg) ~= "table" then return {} end
  return reg
end

local function save_registry(reg)
  global.set(REGISTRY_KEY, reg)
end

local function is_registered(climate_entity)
  return type(climate_entity) == "string" and load_registry()[climate_entity] ~= nil
end

-- ---------------------------------------------------------------------------
-- Per-climate dynamic state lives in this script's own KV store, keyed by
-- climate entity id.
-- ---------------------------------------------------------------------------

local function sched_key(climate) return "schedule:" .. climate end
local function override_key(climate) return "override:" .. climate end
local function manual_key(climate) return "manual:" .. climate end
local function override_temp_key(climate) return "override_temp:" .. climate end
local function desired_key(climate) return "desired:" .. climate end
-- What the controller last COMMANDED, as against what the user asked for. The
-- two differ once the overshoot correction cuts a warmup short
-- (overshoot-spec.md §7), and anything comparing against the value on the
-- device must read this one or it reads our own correction as a dial nudge.
local function written_key(climate) return "written:" .. climate end
local function restore_key(climate) return "restore:" .. climate end

-- climate.living_room -> living_room, so the card can derive the companion id
-- rather than having it configured.
local function slug_of(climate)
  return (climate:gsub("^climate%.", ""))
end

local now_parts, parse_time = climate_lib.now_parts, climate_lib.parse_time

local function override_temp(climate)
  local value = store.get(override_temp_key(climate))
  if type(value) == "number" then return value end
  return DEFAULT_OVERRIDE_TEMP
end

local mode, current_target = climate_lib.mode, climate_lib.target
local current_temp = climate_lib.current_temp
local temp_bounds = climate_lib.bounds

-- The entity's friendly_name, falling back to the id while it has none.
local function friendly_name(climate)
  local state = ha.get_state(climate)
  if state and state.attributes and type(state.attributes.friendly_name) == "string" then
    return state.attributes.friendly_name
  end
  return climate
end

local function load_schedule(climate)
  local stored = store.get(sched_key(climate))
  if type(stored) == "table" and type(stored.days) == "table" then return stored.days end
  return {}
end

-- The window sensors the card bound to this climate at configure time.
local function window_sensors_of(climate)
  local cfg = load_registry()[climate]
  if cfg == nil or type(cfg.window_sensors) ~= "table" then return {} end
  return cfg.window_sensors
end

-- Open if ANY bound sensor reads "on"; an unseeded one counts as closed.
local function window_open(climate)
  local states = {}
  for _, sensor in ipairs(window_sensors_of(climate)) do
    local state = ha.get_state(sensor)
    states[#states + 1] = state and state.state or "off"
  end
  return control.window_open(states)
end

-- Suppresses manual-change detection until every bound sensor has seeded.
local function window_unknown(climate)
  for _, sensor in ipairs(window_sensors_of(climate)) do
    if ha.get_state(sensor) == nil then return true end
  end
  return false
end

-- The live timed override, or nil, clearing an expired one so the climate
-- reverts to schedule.
local function active_override(climate, now)
  local override = store.get(override_key(climate))
  if type(override) ~= "table" or not override.active or type(override.ends_at) ~= "string" then
    return nil
  end
  local ends = parse_time(override.ends_at)
  if ends == nil then
    store.delete(override_key(climate))
    return nil
  end
  if now:before(ends) then return override end
  store.delete(override_key(climate))
  return nil
end

-- The live manual hold, or nil, clearing it once `expires` has passed. ("until"
-- would be a Lua keyword.)
local function active_manual(climate, now)
  local manual = store.get(manual_key(climate))
  if type(manual) ~= "table" or type(manual.temp) ~= "number" or type(manual.expires) ~= "string" then
    return nil
  end
  local exp = parse_time(manual.expires)
  if exp == nil then
    store.delete(manual_key(climate))
    return nil
  end
  if now:before(exp) then return manual end
  store.delete(manual_key(climate))
  return nil
end

-- override > manual > schedule via control.desired, resolving each source's
-- candidate and expiring stale holds on the way.
local function desired(climate, now, dow, minute)
  local override = active_override(climate, now) and override_temp(climate) or nil
  local manual = active_manual(climate, now)
  local sched_temp = schedule.resolve(load_schedule(climate), dow, minute)
  return control.desired(override, manual and manual.temp or nil, sched_temp)
end

-- ---------------------------------------------------------------------------
-- Overshoot correction (overshoot-spec.md): heat on demand, cut on evidence.
-- lib/overshoot.lua holds the math; the episode lives here because store.* is
-- per-script and the cut has to reach the device through this controller.
--
-- A learner that discards every episode looks exactly like one that has
-- converged, which is why every episode is journaled with its reason and
-- discards log at warn.
-- ---------------------------------------------------------------------------

-- A new key rather than the old overshoot_k: that one held v4.13's {base,
-- slope}, which means something else entirely.
local function c_key(climate) return "overshoot_c:" .. climate end
-- Counts runs of the model c belongs to; the old key counted v4.13's runs.
local function samples_key(climate) return "overshoot_c_samples:" .. climate end
local function episode_key(climate) return "overshoot_episode:" .. climate end
local function journal_key(climate) return "overshoot_journal:" .. climate end
local function observe_key(climate) return "overshoot_observe:" .. climate end

local JOURNAL_MAX = 50

-- Zero until learned: the prediction is then the room itself, so the earliest
-- cut is the moment the room reaches the request — never before the node's own.
local function learned_c(climate)
  local value = store.get(c_key(climate))
  if type(value) == "number" then return value end
  return 0
end

-- How many runs c was learned from: a coefficient alone says nothing about
-- whether to trust it.
local function learned_samples(climate)
  local value = store.get(samples_key(climate))
  if type(value) == "number" then return value end
  return 0
end

local function live_episode(climate)
  local episode = store.get(episode_key(climate))
  if type(episode) ~= "table" then return nil end
  return episode
end

-- Unset means observe-only is ON: the correction ships watching rather than
-- acting, so it can be judged on a week of what it would have done (§9.4).
local function observe_only(climate)
  return store.get(observe_key(climate)) ~= false
end

-- A numeric sensor state, or nil for an unseeded, unavailable or non-numeric
-- one. Missing is recorded as missing; a zero would read as a freezing radiator.
local function sensor_number(entity)
  if type(entity) ~= "string" or entity == "" then return nil end
  local state = ha.get_state(entity)
  if state == nil then return nil end
  return tonumber(state.state)
end

-- Both come from the card config, like everything else here: this script is
-- provisioned at runtime and deliberately has nothing to edit in the file.
local function radiator_of(climate)
  local cfg = load_registry()[climate]
  return cfg and cfg.radiator_entity or nil
end

-- The outdoor sensor is per-climate only because that is where the config lives;
-- in practice every card points at the same one. A daily-average sensor beats an
-- instantaneous reading here: the plant is slow enough that the momentary value
-- at 06:00 is mostly noise.
local function outdoor_of(climate)
  local cfg = load_registry()[climate]
  return cfg and cfg.outdoor_entity or nil
end

-- The conditions an episode ran under, sampled fresh each tick because the
-- radiator is the evidence the cut is decided on: it swings 30° across one run.
-- `heating` is the relay, which is what says a run actually happened.
local function env_snapshot(climate)
  return {
    outdoor = sensor_number(outdoor_of(climate)),
    radiator = sensor_number(radiator_of(climate)),
    heating = climate_lib.heating(climate),
  }
end

local function journal(climate, record)
  local rows = store.get(journal_key(climate))
  if type(rows) ~= "table" then rows = {} end
  rows[#rows + 1] = record
  while #rows > JOURNAL_MAX do table.remove(rows, 1) end
  store.set(journal_key(climate), rows)
end

local function open_episode(climate, requested, current, at, env)
  local watching = observe_only(climate)
  local episode = overshoot.open(requested, current, learned_c(climate), watching, at, env)
  if episode == nil then return nil end
  store.set(episode_key(climate), episode)
  ha.log("info", string.format(
    "overshoot %s: run started requested=%.1f room=%.1f radiator=%s outdoor=%s c=%.3f%s",
    climate, requested, current, tostring(episode.radiator_at_open),
    tostring(episode.outdoor_at_open), episode.c_used, watching and " (observe-only)" or ""))
  return episode
end

local function close_episode(climate, episode, at)
  local c_before = learned_c(climate)
  local c_after, outcome, reason = overshoot.close(episode, c_before)
  if outcome == "discarded" then
    -- warn, not debug: needing to raise the log level to notice the learner has
    -- never once run would defeat the point of journaling it.
    ha.log("warn", string.format(
      "overshoot %s: discarded (%s) requested=%.1f room=%.1f peak=%.1f",
      climate, reason, episode.requested, episode.current_at_open, episode.peak))
  else
    store.set(c_key(climate), c_after)
    store.set(samples_key(climate), learned_samples(climate) + 1)
    local half = overshoot.half_life(episode)
    ha.log("info", string.format(
      "overshoot %s: %s cut_by=%s lead=%.1f peak=%.2f requested=%.1f error=%+.2f cool_half_life=%s c %.3f -> %.3f (observed %.3f)",
      climate, outcome, episode.cut_by, episode.lead_at_cutoff, episode.peak, episode.requested,
      episode.peak - episode.requested, half and string.format("%.0fs", half) or "n/a",
      c_before, c_after, episode.c_observed))
  end
  journal(climate, overshoot.record(episode, climate, c_before, c_after, outcome, reason, at))
  store.delete(episode_key(climate))
end

-- The three decisions a run can make, logged as they happen rather than only in
-- the journal row at the end: "why did the heating stop at 23.1" has to be
-- answerable from the log while the room is still coasting.
local function log_decisions(climate, episode, before)
  if episode.cutoff_at ~= nil and before.cutoff_at == nil and episode.cut_by == "overshoot" then
    ha.log("info", string.format(
      "overshoot %s: cut early room=%.1f radiator=%.1f lead=%.1f predicted=%.2f requested=%.1f hold=%.1f",
      climate, episode.room_at_cutoff, episode.radiator_at_cutoff, episode.lead_at_cutoff,
      episode.predicted_at_cutoff, episode.requested, episode.hold_temp))
  end
  if episode.would_cut_at ~= nil and before.would_cut_at == nil then
    ha.log("info", string.format(
      "overshoot %s: would cut now (observe-only) lead=%.1f predicted=%.2f requested=%.1f",
      climate, episode.would_cut_lead, episode.would_cut_predicted, episode.requested))
  end
  if episode.released_at ~= nil and before.released_at == nil then
    ha.log("info", string.format(
      "overshoot %s: hold released, the stored heat will not reach %.1f",
      climate, episode.requested))
  end
end

-- Closes a live episode without a new observation, for the paths that leave the
-- climate uncontrolled entirely: a boost expiring with no schedule under it has
-- no request left to judge the run against.
local function abandon_episode(climate, now, reason)
  local episode = live_episode(climate)
  if episode == nil then return end
  overshoot.invalidate(episode, reason)
  close_episode(climate, episode, now:unix())
end

-- The learner's numbers for the companion payload and the Ingress page.
-- `holding` is a cut in force right now; `would_hold` is observe-only's
-- equivalent, true from the moment it would have cut until the run ends.
local function overshoot_status(climate)
  local episode = live_episode(climate)
  return {
    c = learned_c(climate),
    samples = learned_samples(climate),
    holding = episode ~= nil and episode.hold == true,
    would_hold = episode ~= nil and episode.observe_only and episode.would_cut_at ~= nil,
    observe_only = observe_only(climate),
  }
end

-- Advances the climate's episode by one observation and returns the setpoint to
-- command: the request, except while an armed cut holds the run off.
--
-- A run's episode opens when the relay is on with none running, or when the
-- request changes to above the room (for a device that reports no relay). Not
-- whenever the room sits below the setpoint, which is true on every tick of a
-- hold and would open an episode a minute.
local function overshoot_step(climate, now, requested, previous)
  local at = now:unix()
  local current = current_temp(climate)
  local heating = mode(climate) == "heat"
  local window = window_open(climate)
  local requested_changed = requested ~= previous
  local env = env_snapshot(climate)
  local relay_on = env.heating == true

  local episode = live_episode(climate)

  if episode ~= nil then
    if requested_changed then overshoot.invalidate(episode, "setpoint_changed") end
    if not heating then overshoot.invalidate(episode, "mode_left_heat") end
    if window then overshoot.invalidate(episode, "window_open") end
    -- Switching observe-only mid-run: half of it ran under the other setting and
    -- cannot be judged as either.
    if episode.observe_only ~= observe_only(climate) then
      overshoot.invalidate(episode, "observe_changed")
    end
    local before = {
      cutoff_at = episode.cutoff_at,
      would_cut_at = episode.would_cut_at,
      released_at = episode.released_at,
    }
    local phase = "heating"
    if current ~= nil then phase = overshoot.step(episode, current, at, env) end
    log_decisions(climate, episode, before)
    if episode.invalid ~= nil or phase == "done" then
      close_episode(climate, episode, at)
      episode = nil
    else
      store.set(episode_key(climate), episode)
    end
  end

  if episode == nil and (requested_changed or relay_on) and heating and not window and current ~= nil then
    episode = open_episode(climate, requested, current, at, env)
  end

  if episode == nil or not episode.hold then return requested end
  -- Never above the request: the hold only ever stops heating, it cannot add any.
  local lo, hi = temp_bounds(climate)
  return control.clamp_bounds(math.min(episode.hold_temp, requested), lo, hi)
end

local function set_temp(climate, temp)
  ha.log("info", "set_temperature " .. climate .. " = " .. tostring(temp) .. "°")
  ha.call_service("climate", "set_temperature", { entity_id = climate, temperature = temp })
end

-- Writes the companion entity (§6) the card reads: state is the REQUESTED
-- setpoint when controlled, else "off", with the schedule, override, manual,
-- window and preset detail in its attributes. desired_temp arrives already
-- clamped.
--
-- `state` and the `commanded` attribute are deliberately separate: the request
-- is what the user asked for and the primary number everywhere, while
-- `commanded` is what the device was actually told, which sits below the
-- request while the overshoot correction cuts a warmup short.
local function publish_companion(climate, now, desired_temp)
  local cfg = load_registry()[climate]
  if cfg == nil then return end
  local lo, hi = temp_bounds(climate)
  local friendly = friendly_name(climate)

  local override_tbl = { active = false }
  local override = active_override(climate, now)
  if override then
    override_tbl = { active = true, expires = override.ends_at, temp = override_temp(climate) }
  end

  local manual_tbl = { active = false }
  local manual = active_manual(climate, now)
  if manual then
    manual_tbl = { active = true, ["until"] = manual.expires } -- "until" is a keyword
  end

  local controlled = desired_temp ~= nil
  local state_value = controlled and desired_temp or "off"

  local attrs = {
    ha_lua_climate = climate,
    friendly_name = friendly,
    schedule = load_schedule(climate),
    override = override_tbl,
    override_temp = override_temp(climate), -- always surfaced so the card can edit it
    manual = manual_tbl,
    -- What is actually on the device, read back rather than taken from
    -- `written`: it then also shows the frost setpoint while a window is open,
    -- and exposes a write that never landed (overshoot-spec.md §8).
    commanded = current_target(climate),
    overshoot = overshoot_status(climate),
    window = { sensors = window_sensors_of(climate), open = window_open(climate) },
    presets = cfg.presets,
    min_temp = lo,
    max_temp = hi,
    controlled = controlled,
    unit_of_measurement = "°C",
    device_class = "temperature",
    icon = "mdi:thermostat",
    removal = "Deleting the card keeps this running — remove it in the ha-lua panel",
  }

  -- Every set_state is a state_changed event and a recorder row, so an unchanged
  -- sensor is not rewritten each minute. The slow heartbeat still re-publishes
  -- unchanged data, because these states are not integration-backed and an HA
  -- restart drops them; a blind periodic write beats probing for our own sensor.
  local snapshot = json.encode({ state = state_value, attrs = attrs })
  local prev = published[climate]
  if prev and prev.snapshot == snapshot and (os.time() - prev.at) < PUBLISH_HEARTBEAT then
    return
  end

  -- set_state is non-raising, so an outage is invisible unless logged here.
  local created, err = card.publish(slug_of(climate), state_value, attrs)
  if err then
    ha.log("warn", "publish companion for " .. climate .. " failed: " .. err)
  else
    published[climate] = { snapshot = snapshot, at = os.time() }
    if created then
      ha.log("info", "published companion sensor for " .. climate)
    else
      ha.log("debug", "refreshed companion for " .. climate .. " (state " .. tostring(state_value) .. ")")
    end
  end
end

-- The per-climate control step: compute the desired setpoint, clamp it, remember
-- it, and write it when the shared gate allows. While any bound window is open
-- the frost setpoint is held instead, and the remembered desired is restored
-- once they all close. Configure, tick and every mutation refresh the companion
-- through this one path.
--
-- Two values are remembered, not one: `desired` is what was asked for, `written`
-- is what the device was commanded to. Manual detection compares against
-- `written`.
local function apply_climate(climate, now, dow, minute)
  local desired_temp, source = desired(climate, now, dow, minute)
  -- The pre-boost snapshot matters only while the boost is the active source:
  -- anything else takes the climate over from here.
  local previous = nil
  if source ~= "override" then
    previous = store.get(restore_key(climate))
    if previous ~= nil then store.delete(restore_key(climate)) end
  end
  local lo, hi = temp_bounds(climate)
  if desired_temp ~= nil then
    desired_temp = control.clamp_bounds(desired_temp, lo, hi)
    -- Read before the publish below overwrites it: a request differing from the
    -- last one published is what opens an overshoot episode.
    local last_requested = store.get(desired_key(climate))
    local commanded_temp = overshoot_step(climate, now, desired_temp, last_requested)
    store.set(desired_key(climate), desired_temp)
    if mode(climate) == "heat" then
      local current = current_target(climate)
      if window_open(climate) then
        commanded_temp = control.clamp_bounds(FROST_TEMP, lo, hi)
        if current == nil or math.abs(current - commanded_temp) > 0.05 then
          set_temp(climate, commanded_temp)
        end
      elseif control.should_write("heat", false, current, commanded_temp) then
        set_temp(climate, commanded_temp)
      end
      store.set(written_key(climate), commanded_temp)
    elseif store.get(written_key(climate)) == nil then
      -- Nothing is written outside heat, so `written` keeps what the device was
      -- last told: recording the request here latched our frost or hold as a
      -- dial change once heat came back. Unset (configured while off), the
      -- device's own setpoint is the baseline, or heat's first event would read
      -- as the user.
      store.set(written_key(climate), current_target(climate))
    end
  else
    -- Nothing is requested any more, so a live episode has no target left to be
    -- judged against.
    abandon_episode(climate, now, "setpoint_changed")
    -- A boost ending with no schedule or hold under it must still put the dial
    -- back where it found it, or the boost temperature sticks forever.
    local restored = type(previous) == "number" and control.clamp_bounds(previous, lo, hi) or nil
    if restored and control.should_write(mode(climate), false, current_target(climate), restored) then
      set_temp(climate, restored)
      -- Our own write must not read as a dial nudge, and the detector compares
      -- against `written`.
      store.set(desired_key(climate), restored)
      store.set(written_key(climate), restored)
    else
      store.delete(desired_key(climate)) -- not controlled (no schedule/override/manual)
      store.delete(written_key(climate))
    end
  end
  publish_companion(climate, now, desired_temp)
end

-- Shared by the tick and the load-time resume, so both take one path.
local function apply_all(now, dow, minute)
  for climate_entity in pairs(load_registry()) do
    apply_climate(climate_entity, now, dow, minute)
  end
end

-- Override/manual expiry happens inside desired().
local function tick()
  apply_all(now_parts())
end

ha.every("1m", tick)

-- ---------------------------------------------------------------------------
-- Manual setpoint change detection (§7.2): this controller is the only thing that
-- writes the setpoint, and always writes what it recorded as `written`, so a
-- target differing from that is the user at the dial. It becomes an ad-hoc manual hold
-- lasting until the next schedule transition. One wildcard handler, because
-- climates are registered at runtime and a load-time registration cannot see
-- them.
-- ---------------------------------------------------------------------------

-- Judges a climate state change as the user at the dial and records the hold.
-- Returns whether it did.
local function manual_change(climate_entity, new_state, now, dow, minute)
  local target = new_state.attributes.temperature
  if type(target) ~= "number" then return false end
  if active_override(climate_entity, now) then return false end -- override wins; ignore nudges
  -- A window open (or not yet seeded) means we may have written the frost
  -- setpoint, which must not be mistaken for a user dial change.
  if window_open(climate_entity) or window_unknown(climate_entity) then return false end

  local last_written = store.get(written_key(climate_entity))
  if not control.is_manual(target, last_written) then return false end

  local _, _, mins_to_next = schedule.resolve(load_schedule(climate_entity), dow, minute)
  local hold = mins_to_next ~= nil and mins_to_next * 60 or 24 * 3600
  store.set(manual_key(climate_entity), {
    temp = target,
    expires = now:add(hold):format(time.RFC3339),
  })
  ha.log("info", "manual change on " .. climate_entity .. " -> " .. tostring(target) .. "° (held to next transition)")
  return true
end

ha.on_state_change("climate.*", function(data)
  local climate_entity = data.entity_id
  if not is_registered(climate_entity) then return end
  local new_state = data.new_state
  if new_state == nil or new_state.attributes == nil then return end
  if new_state.state ~= "heat" then return end
  local now, dow, minute = now_parts()

  -- The dial is judged BEFORE anything writes: re-applying below can change the
  -- setpoint, and that write would read as a dial change against the target
  -- this very event carries.
  local held = manual_change(climate_entity, new_state, now, dow, minute)
  -- The relay closing opens a cycle's episode (overshoot-spec.md §5). The tick
  -- would see it up to a minute later, but the offset is latched at the open
  -- and the cut can only be as early as that.
  local old_attrs = data.old_state and data.old_state.attributes or {}
  local relay_closed = new_state.attributes.hvac_action == "heating"
    and old_attrs.hvac_action ~= "heating"
  -- Nothing is written outside heat, so the request is due the moment it returns.
  local entered_heat = data.old_state == nil or data.old_state.state ~= "heat"
  if held or relay_closed or entered_heat then
    apply_climate(climate_entity, now, dow, minute) -- republish at once
  end
end)

-- A bound window opening or closing re-applies its climate within seconds rather
-- than at the next tick. Wildcard for the same reason as above.
ha.on_state_change("binary_sensor.*", function(data)
  local sensor = data.entity_id
  if type(sensor) ~= "string" then return end
  local now, dow, minute = now_parts()
  for climate_entity in pairs(load_registry()) do
    for _, bound in ipairs(window_sensors_of(climate_entity)) do
      if bound == sensor then
        ha.log("info", "window " .. sensor .. " (" .. tostring(data.new_state and data.new_state.state) ..
          ") changed -> re-applying " .. climate_entity)
        apply_climate(climate_entity, now, dow, minute)
        break
      end
    end
  end
end)

-- ---------------------------------------------------------------------------
-- Command handlers (card → daemon, §5). Each validates, mutates only when valid,
-- then re-applies the climate; a rejected command leaves state unchanged and the
-- card snaps back from the next hass update.
-- ---------------------------------------------------------------------------

-- Defaults the optional lists, so later code never type-checks them.
--
-- `radiator_entity` is the sensor strapped to this zone's radiator, and
-- `outdoor_entity` the house's outdoor temperature. Both are recorded with every
-- overshoot episode and neither is acted on: the radiator temperature at the
-- cutoff IS the stored energy about to land in the room, and the pair is what
-- will eventually say whether one coefficient per climate is enough
-- (overshoot-spec.md §12). Empty string means none.
local function normalize(data)
  return {
    climate_entity = data.climate_entity,
    window_sensors = type(data.window_sensors) == "table" and data.window_sensors or {},
    presets = type(data.presets) == "table" and data.presets or {},
    radiator_entity = type(data.radiator_entity) == "string" and data.radiator_entity or "",
    outdoor_entity = type(data.outdoor_entity) == "string" and data.outdoor_entity or "",
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

-- So configure can no-op: a card re-firing on every tab focus must not thrash
-- the registry.
local function config_equal(x, y)
  if x == nil or y == nil then return x == y end
  return x.climate_entity == y.climate_entity
      and list_equal(x.window_sensors, y.window_sensors)
      and list_equal(x.presets, y.presets)
      and (x.radiator_entity or "") == (y.radiator_entity or "")
      and (x.outdoor_entity or "") == (y.outdoor_entity or "")
end

-- Idempotent upsert, fired by the card on load and on any config change.
card.on("configure", function(data)
  if type(data) ~= "table" or type(data.climate_entity) ~= "string" or data.climate_entity == "" then
    return
  end
  local cfg = normalize(data)
  local reg = load_registry()
  if not config_equal(reg[cfg.climate_entity], cfg) then
    reg[cfg.climate_entity] = cfg
    save_registry(reg)
    ha.log("info", "configure " .. cfg.climate_entity ..
      " (windows: " .. #cfg.window_sensors .. ", presets: " .. #cfg.presets ..
      ", radiator: " .. (cfg.radiator_entity ~= "" and cfg.radiator_entity or "none") .. ")")
  end
  -- Republish even when the config was unchanged: the card sends configure
  -- precisely when it does NOT see a matching companion, so clearing the dedup
  -- cache is what makes the companion reappear and ends the card's reconcile
  -- loop instead of it re-sending configure forever.
  published[cfg.climate_entity] = nil
  local now, dow, minute = now_parts()
  apply_climate(cfg.climate_entity, now, dow, minute) -- start controlling at once
end)

-- Deprovisions an enhanced climate, for both the card's remove command and the
-- Ingress removal page.
local function remove_climate(climate)
  if type(climate) ~= "string" then return end
  local reg = load_registry()
  if reg[climate] == nil then return end
  reg[climate] = nil
  save_registry(reg)
  store.delete(desired_key(climate))
  store.delete(written_key(climate))
  store.delete(restore_key(climate)) -- a re-add must not resurrect a pre-boost setpoint
  -- A re-added climate starts learning from scratch rather than inheriting a c
  -- measured on a plant that may since have been replumbed.
  store.delete(c_key(climate))
  store.delete(samples_key(climate))
  store.delete(episode_key(climate))
  store.delete(journal_key(climate))
  store.delete(observe_key(climate))
  published[climate] = nil -- a re-add must re-publish, not skip against the stale cache
  local _, err = card.remove(slug_of(climate)) -- the companion disappears with it
  if err then
    ha.log("warn", "remove companion for " .. climate .. " failed: " .. err)
  else
    ha.log("info", "removed enhanced climate " .. climate)
  end
end

-- Deleting the card does NOT fire this: removal is deliberately explicit (§8).
card.on("remove", function(data)
  if type(data) ~= "table" then return end
  remove_climate(data.climate_entity)
end)

-- Replaces the 7-day schedule, bounded by the device's range.
card.on("schedule", function(data)
  local climate = data.climate_entity
  if not is_registered(climate) then return end
  local lo, hi = temp_bounds(climate)
  if not schedule.validate(data.schedule, lo, hi) then return end
  store.set(sched_key(climate), { days = data.schedule })
  ha.log("info", "schedule updated for " .. climate)
  local now, dow, minute = now_parts()
  apply_climate(climate, now, dow, minute)
end)

-- override starts or cancels a timed override (a boost to the override temp).
card.on("override", function(data)
  local climate = data.climate_entity
  if not is_registered(climate) then return end
  local now, dow, minute = now_parts()
  if data.cancel then
    store.delete(override_key(climate))
    ha.log("info", "override cancelled for " .. climate)
  else
    if type(data.minutes) ~= "number" or data.minutes <= 0 or data.minutes > 1440 then return end
    -- Once per boost, before it is overwritten: extending a running boost must
    -- not snapshot the boost temperature as the way back. Only an uncontrolled
    -- climate falls back on it; a controlled one has its source under the boost.
    if not active_override(climate, now) then
      local current = current_target(climate)
      if type(current) == "number" then store.set(restore_key(climate), current) end
    end
    store.set(override_key(climate), {
      active = true,
      ends_at = now:add(data.minutes * 60):format(time.RFC3339),
    })
    -- The dial hold stays: the boost outranks it and it takes back over after.
    -- Deleting it left an uncontrolled climate whose way back was the snapshot,
    -- and with a window open that snapshot is our own frost.
    -- The tick would notice up to a minute late, which the card shows as a
    -- countdown frozen at 00:00 and a boost that refuses to finish. The tick
    -- stays as the backstop for an ha.after lost to a restart.
    ha.after(string.format("%gm", data.minutes), function()
      local ends_now, ends_dow, ends_minute = now_parts()
      apply_climate(climate, ends_now, ends_dow, ends_minute)
    end)
    ha.log("info", "override " .. data.minutes .. "m for " .. climate)
  end
  apply_climate(climate, now, dow, minute)
end)

-- The card's half of §9.5/§9.4: reset a learned coefficient, or take a climate
-- out of observe-only. Same two writes as the HTTP endpoints, reached over the
-- card's own channel so the panel needs no Ingress URL.
card.on("overshoot", function(data)
  local climate = data.climate_entity
  if not is_registered(climate) then return end
  if data.reset then
    -- The in-flight episode goes too: it would close against a k that no longer
    -- exists and journal a row nobody could account for.
    store.delete(episode_key(climate))
    store.delete(c_key(climate))
    store.delete(samples_key(climate))
    store.delete(journal_key(climate))
    ha.log("warn", "overshoot " .. climate .. ": c and journal reset")
  elseif type(data.observe_only) == "boolean" then
    store.set(observe_key(climate), data.observe_only)
    ha.log("warn", string.format("overshoot %s: observe_only = %s", climate, tostring(data.observe_only)))
  else
    return
  end
  local now, dow, minute = now_parts()
  apply_climate(climate, now, dow, minute)
end)

-- Edits the temperature a boost jumps to, bounded by the device's range.
card.on("settings", function(data)
  local climate = data.climate_entity
  if not is_registered(climate) then return end
  local lo, hi = temp_bounds(climate)
  if type(data.override_temp) ~= "number" or data.override_temp < lo or data.override_temp > hi then
    return
  end
  store.set(override_temp_key(climate), data.override_temp)
  ha.log("info", "override_temp set to " .. data.override_temp .. "° for " .. climate)
  local now, dow, minute = now_parts()
  apply_climate(climate, now, dow, minute) -- if an override is active, the new temp applies now
end)

-- ---------------------------------------------------------------------------
-- Ingress removal page (§8). An enhanced climate outlives any card, so removal
-- lives here: a page listing the registry with a remove button, covering both
-- deliberate teardown and orphans, since a deleted card cannot send remove.
-- ---------------------------------------------------------------------------

local JSON_HDR = { ["Content-Type"] = "application/json" }
local TEXT_HDR = { ["Content-Type"] = "text/plain" }

-- The registry as a flat array for the page.
local function list_climates()
  local out = {}
  for climate, cfg in pairs(load_registry()) do
    out[#out + 1] = {
      climate_entity = climate,
      name = friendly_name(climate),
      window_sensors = cfg.window_sensors or {},
      presets = cfg.presets or {},
      radiator_entity = cfg.radiator_entity or "",
      outdoor_entity = cfg.outdoor_entity or "",
      overshoot = overshoot_status(climate),
    }
  end
  return out
end

ha.serve("GET", "/api/list", function()
  return 200, json.encode({ climates = list_climates() }), JSON_HDR
end)

ha.serve("POST", "/api/remove", function(req)
  local ok, body = pcall(json.decode, req.body)
  if not ok or type(body) ~= "table" or type(body.climate_entity) ~= "string" then
    return 400, "invalid JSON body", TEXT_HDR
  end
  remove_climate(body.climate_entity)
  return 200, json.encode({ climates = list_climates() }), JSON_HDR
end)

-- ---------------------------------------------------------------------------
-- Overshoot introspection (§9.5, §9.6). The learner is the only thing here that
-- fails SILENTLY — it accumulates a number over days from episodes nobody
-- watched, and a wrong one shows up as a room quietly too cold in February. So
-- c, the journal and the two writes are curl-able, and recovering from a bad c
-- must never be `sqlite3 /data/ha-lua.db` or a restart.
-- ---------------------------------------------------------------------------

-- The live episode is included so "why is it commanding that" is answerable
-- while it is still happening, not only afterwards from the journal.
local function overshoot_report(climate)
  local status = overshoot_status(climate)
  status.climate_entity = climate
  status.name = friendly_name(climate)
  status.episode = live_episode(climate)
  status.journal = store.get(journal_key(climate)) or {}
  return status
end

ha.serve("GET", "/api/overshoot", function(req)
  local climate = req.query.climate
  if not is_registered(climate) then
    return 404, "unknown climate: " .. tostring(climate), TEXT_HDR
  end
  return 200, json.encode(overshoot_report(climate)), JSON_HDR
end)

-- Decodes a body that must name a registered climate; returns nil plus the
-- response triple to hand straight back.
local function climate_from_body(req)
  local ok, body = pcall(json.decode, req.body)
  if not ok or type(body) ~= "table" then
    return nil, 400, "invalid JSON body", TEXT_HDR
  end
  if not is_registered(body.climate_entity) then
    return nil, 404, "unknown climate: " .. tostring(body.climate_entity), TEXT_HDR
  end
  return body
end

ha.serve("POST", "/api/overshoot/reset", function(req)
  local body, status, text, hdr = climate_from_body(req)
  if body == nil then return status, text, hdr end
  local climate = body.climate_entity
  -- The in-flight episode goes too: it would close against a k that no longer
  -- exists and journal a row nobody could account for.
  store.delete(episode_key(climate))
  store.delete(c_key(climate))
  store.delete(samples_key(climate))
  store.delete(journal_key(climate))
  -- warn: zeroing a learned coefficient is a deliberate act and the log is where
  -- a later "why did it forget everything" gets answered.
  ha.log("warn", "overshoot " .. climate .. ": c and journal reset")
  local now, dow, minute = now_parts()
  apply_climate(climate, now, dow, minute) -- republish the companion at once
  return 200, json.encode(overshoot_report(climate)), JSON_HDR
end)

ha.serve("POST", "/api/overshoot/observe", function(req)
  local body, status, text, hdr = climate_from_body(req)
  if body == nil then return status, text, hdr end
  if type(body.observe_only) ~= "boolean" then
    return 400, "observe_only must be a boolean", TEXT_HDR
  end
  local climate = body.climate_entity
  store.set(observe_key(climate), body.observe_only)
  -- Trusting the correction with a real room is worth a warn, not a debug line.
  ha.log("warn", string.format("overshoot %s: observe_only = %s", climate, tostring(body.observe_only)))
  -- Closes a running episode through the step function's observe_changed check
  -- rather than a minute later.
  local now, dow, minute = now_parts()
  apply_climate(climate, now, dow, minute)
  return 200, json.encode(overshoot_report(climate)), JSON_HDR
end)

local PAGE = assert(fs.read("enhanced_climate.html"),
  "enhanced_climate.html missing next to enhanced_climate.lua")

ha.ui("Climate")
ha.serve("GET", "/", function()
  return 200, PAGE, { ["Content-Type"] = "text/html; charset=utf-8" }
end)

-- Re-publish at load, before the first tick: an HA restart drops REST-set states,
-- so this is what makes the companions reappear.
do
  local now = now_parts()
  local count = 0
  for climate in pairs(load_registry()) do
    count = count + 1
    -- An episode in flight when the daemon stopped is abandoned, not resumed:
    -- its timing is broken, and one lost sample costs less than a corrupted
    -- coefficient (spec §6). Journaled, so the gap is visible.
    abandon_episode(climate, now, "restart")
  end
  ha.log("info", "enhanced_climate loaded, resuming " .. count .. " climate(s)")
end
apply_all(now_parts())

ha.on_exception(ha.exceptions.log_file("enhanced-climate-errors.log"))
