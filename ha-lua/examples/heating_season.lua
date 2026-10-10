-- heating_season.lua
--
-- Heating on and off for the season, from the 3-day outdoor average. Heating
-- comes on the first time the average is below ON_BELOW, which arms ONE
-- switch-off: the next OFF_AT with the average above OFF_ABOVE. After that the
-- script leaves the climates alone until the average drops below ON_BELOW
-- again, so heating turned back on by hand for a cold night stays on. An
-- automation that switches off every morning the average sits above OFF_ABOVE
-- fights whoever keeps turning it back on.
--
-- Off only at OFF_AT, never going into a cold night.

local AVERAGE = "sensor.kinti_3_napos_atlag"
local ON_BELOW = 14
local OFF_ABOVE = 17
local OFF_AT = "10:00"

local CLIMATES = {
  "climate.konyha_furdo_futes",
  "climate.konyha_nappali_futes",
  "climate.konyha_halo_futes",
  "climate.konyha_gyerekszoba_futes",
}

-- Persisted so a restart neither re-sends heat nor forgets an armed off.
local season = store.state({ off_armed = false })

local function average()
  local state = ha.get_state(AVERAGE)
  return state and tonumber(state.state)
end

-- Called before the flag flips: a failed call is retried on the next reading
-- or the next morning instead of being lost.
local function set_mode(mode)
  ha.call_service("climate", "set_hvac_mode", { entity_id = CLIMATES, hvac_mode = mode })
end

-- initial: a drop below ON_BELOW while the daemon was down still turns heat on.
ha.on_state_change(AVERAGE, function()
  local outdoor = average()
  if season.off_armed or outdoor == nil or outdoor >= ON_BELOW then return end
  set_mode("heat")
  season.off_armed = true
  ha.log("info", string.format("heating_season: average %.2f below %g, heating on", outdoor, ON_BELOW))
end, { initial = true })

ha.at(OFF_AT, function()
  local outdoor = average()
  if not season.off_armed or outdoor == nil or outdoor <= OFF_ABOVE then return end
  set_mode("off")
  season.off_armed = false
  ha.log("info", string.format("heating_season: average %.2f above %g, heating off", outdoor, OFF_ABOVE))
end)

if not ha.get_state(AVERAGE) then
  ha.log("warn", AVERAGE .. " is unknown to the daemon — is that the right entity id?")
end

ha.on_exception(ha.exceptions.log_file("heating_season-errors.log"))
