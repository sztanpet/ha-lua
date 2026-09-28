-- lib/overshoot.lua
--
-- Pure heating-overshoot math, free of any ha.* / store.* / time access so it
-- can be unit-tested directly from Go. The controller does the clock and I/O
-- work, keeps the episode table this returns, and feeds it back every tick.
--
-- The problem (overshoot-spec.md): a bang-bang thermostat drives the relay full
-- on until the room reaches setpoint, by which point the actuator still takes
-- minutes to close and the radiator body keeps heating the room for another
-- half hour. A small room has no mass to absorb that, so it sails past.
--
-- Heat on demand, cut on evidence (spec §5). Heating always starts the moment
-- the room is below the setpoint; nothing here ever lowers the setpoint before
-- or at the start of a run. Once the radiator has been SEEN warming, every step
-- predicts where the room ends up if the heat stops now, `room + c * lead`, and
-- cuts when that reaches the request. `c` — room rise after the heat stops, per
-- degree the radiator is above the room at that moment — is measured on every
-- run and smoothed.

local M = {}

-- How far the radiator must climb above its lowest reading in the run before a
-- cut is allowed. Before that the valve may not even be open, and cutting is
-- cancelling the run.
M.RAD_RISE = 1.0
-- The hold sits this far below the room at the cut, so the node stops whichever
-- way it rounds.
M.HOLD_MARGIN = 0.5
-- Fraction of each run's measured c folded into the learned one.
M.GAIN = 0.5
-- At this c a radiator barely past the gate already predicts the request, so
-- the cut comes at the gate: the shortest run the rules allow.
M.C_MAX = 0.2
-- A cutoff with the radiator less than this above the room teaches nothing
-- about stored heat.
M.MIN_LEAD = 3.0
-- The coast ends when the room has turned, not on a timer: 0.2 below its peak
-- (clear of 0.1° flicker), with the peak settled for PEAK_HOLD_SECONDS. The
-- first live radiator took 28 minutes to shed half its lead, so a fixed
-- 30-minute window measured every peak short.
M.PEAK_DROP = 0.2
M.PEAK_HOLD_SECONDS = 5 * 60
-- Backstop for a room that never turns — a sunny window, another heat source —
-- so the episode cannot sit open all afternoon.
M.MAX_COAST_SECONDS = 90 * 60
-- A run that has not cut off in this long is abandoned, so a room the heating
-- cannot satisfy fails loudly instead of staying open forever.
M.MAX_EPISODE_SECONDS = 4 * 3600
-- Ceiling on the coast decay series. The longest coast on a 1-minute tick fills
-- about 90 slots; the cap is what stops a faster tick from growing the journal
-- row without bound.
M.DECAY_MAX_SAMPLES = 100

-- Float error only: a prediction computed to exactly the request must count.
local EPSILON = 1e-6

local function clamp(value, lo, hi)
  if value < lo then return lo end
  if value > hi then return hi end
  return value
end

-- Remembers the relay as `heated` once seen on. Only an explicit false sets
-- false, and only while nothing better is known: a device with no hvac_action
-- leaves it nil, and unknown is not "off".
local function note_heating(episode, env)
  if env == nil then return end
  if env.heating == true then
    episode.heated = true
  elseif env.heating == false and episode.heated == nil then
    episode.heated = false
  end
end

-- Where the room ends up if the heat stops now, or nil with no radiator reading.
-- A radiator at or below the room adds nothing.
function M.predict(c, room, radiator)
  if radiator == nil then return nil end
  return room + c * math.max(0, radiator - room)
end

-- Starts a run, or nil when the room is already above the request (nothing to
-- climb). Nothing is decided here: the correction only ever acts on evidence
-- from the run itself. `at` is epoch seconds.
--
-- `env` is an optional {outdoor =, radiator =, heating =} snapshot. The radiator
-- is the evidence the cut is decided on; the outdoor temperature is recorded
-- only, so the journal can later be tested for what one c per room does not
-- model.
function M.open(requested, room, c, observe_only, at, env)
  if requested - room < 0 then return nil end
  local radiator = env and env.radiator or nil
  local episode = {
    opened_at = at,
    requested = requested,
    current_at_open = room,
    rise = requested - room,
    c_used = c,
    observe_only = observe_only and true or false,
    peak = room,
    peak_at = at,
    radiator_at_open = radiator,
    radiator_min = radiator,
  }
  if env ~= nil then episode.outdoor_at_open = env.outdoor end
  -- Noted at the open too: the relay that opened the run must not be forgotten
  -- if it has already dropped by the first step.
  note_heating(episode, env)
  return episode
end

-- Marks an episode unusable for learning, with one of "window_open",
-- "mode_left_heat", "setpoint_changed", "restart", "never_reached" or
-- "observe_changed" (valid() derives "never_heated", "no_radiator" and
-- "radiator_cold" itself).
--
-- The FIRST reason wins: the one a reader wants is what broke the episode, not
-- what happened to it afterwards.
function M.invalidate(episode, reason)
  if episode.invalid == nil then episode.invalid = reason end
  return episode
end

-- The coast's peak starts fresh at the cutoff: the overshoot is what the room
-- does after the heat stops, and a reading from during the run must not end the
-- coast before the stored heat has landed.
local function mark_cutoff(episode, by, room, at, radiator)
  episode.cutoff_at = at
  episode.cut_by = by
  episode.room_at_cutoff = room
  episode.radiator_at_cutoff = radiator
  if radiator ~= nil then episode.lead_at_cutoff = radiator - room end
  episode.peak, episode.peak_at = room, at
  episode.radiator_at_peak = radiator
end

-- Advances an episode by one observation and reports its phase: "heating" (the
-- run is on), "coasting" (cut off, the stored heat landing) or "done".
--
-- While heating, the cut is either the node's own — the relay seen opening, or
-- for a device that reports no relay, the room past the request — or the
-- correction's, once the radiator is warming and the prediction reaches the
-- request. Observe-only records when it would have cut and lets the run go on,
-- so its cutoff is always the node's own.
--
-- An armed cut sets `hold`: the controller writes `hold_temp` while it lasts.
-- It is released the moment the stored heat can no longer carry the room to the
-- request, so heating resumes as soon as the room needs it.
function M.step(episode, room, at, env)
  local radiator = env and env.radiator or nil
  note_heating(episode, env)
  if room > episode.peak then
    episode.peak, episode.peak_at = room, at
    episode.radiator_at_peak = radiator
  end
  if radiator ~= nil then
    if episode.radiator_min == nil or radiator < episode.radiator_min then
      episode.radiator_min = radiator
    end
    if episode.gate_at == nil and radiator >= episode.radiator_min + M.RAD_RISE then
      episode.gate_at = at
    end
  end

  if episode.cutoff_at == nil then
    local relay_opened = env ~= nil and env.heating == false and episode.heated == true
    local passed = (env == nil or env.heating == nil) and room > episode.requested
    if relay_opened or passed then
      mark_cutoff(episode, "device", room, at, radiator)
      if relay_opened then episode.relay_opened = true end
      M.sample_decay(episode, room, at, radiator)
      return "coasting"
    end
    local predicted = nil
    if episode.gate_at ~= nil then predicted = M.predict(episode.c_used, room, radiator) end
    if predicted ~= nil and predicted >= episode.requested - EPSILON then
      if not episode.observe_only then
        mark_cutoff(episode, "overshoot", room, at, radiator)
        episode.predicted_at_cutoff = predicted
        episode.hold = true
        episode.hold_temp = room - M.HOLD_MARGIN
        M.sample_decay(episode, room, at, radiator)
        return "coasting"
      end
      if episode.would_cut_at == nil then
        episode.would_cut_at = at
        episode.would_cut_predicted = predicted
        episode.would_cut_lead = radiator - room
      end
    end
    if at - episode.opened_at >= M.MAX_EPISODE_SECONDS then
      M.invalidate(episode, "never_reached")
      return "done"
    end
    return "heating"
  end

  M.sample_decay(episode, room, at, radiator)
  -- The relay closing again, once seen open, is the next run: its peak must not
  -- be credited to this one, whatever the sensor's resolution.
  if env ~= nil and env.heating == false then episode.relay_opened = true end
  if env ~= nil and env.heating == true and episode.relay_opened then return "done" end
  if episode.hold then
    local predicted = M.predict(episode.c_used, room, radiator)
    if predicted == nil or predicted < episode.requested - EPSILON then
      episode.hold = false
      episode.released_at = at
    end
  end
  if M.coast_over(episode, room, at) then return "done" end
  return "coasting"
end

-- Whether the coast has ended: the room turned down after its peak, or the
-- backstop ran out. Called after the peak has been updated for this sample, so
-- a new high is never a turn.
function M.coast_over(episode, room, at)
  if at - episode.cutoff_at >= M.MAX_COAST_SECONDS then return true end
  -- The tolerance is float error, not slack: 16.08 - 0.2 lands a hair below 15.88.
  return room <= episode.peak - M.PEAK_DROP + EPSILON
    and at - episode.peak_at >= M.PEAK_HOLD_SECONDS
end

-- Appends one point of the coast decay curve: seconds since the cutoff, the
-- radiator, and the room it is emptying into.
--
-- The cool-down is not a correlate of the overshoot, it IS the overshoot — the
-- heat that lands in the room after the relay drops is the integral of this
-- curve. Two endpoints cannot tell an exponential from a straight line, which
-- is why the middle is kept rather than just a start and an end.
function M.sample_decay(episode, room, at, radiator)
  if radiator == nil or episode.cutoff_at == nil then return end
  if episode.decay == nil then episode.decay = {} end
  if #episode.decay >= M.DECAY_MAX_SAMPLES then return end
  episode.decay[#episode.decay + 1] = {
    t = at - episode.cutoff_at,
    rad = radiator,
    room = room,
  }
end

-- Seconds for the radiator's lead over the room to fall to half what it was at
-- the cutoff, by linear interpolation between the bracketing samples. nil when
-- there is no series, no lead to halve, or it had not halved before the coast
-- ended — "did not halve before the room turned" is itself worth seeing.
--
-- A half-life rather than a fitted time constant: it needs two samples and no
-- regression, it survives a missing reading in the middle, and it does not
-- pretend the decay is a clean exponential when a radiator with a slow valve is
-- not one.
function M.half_life(episode)
  local series = episode.decay
  if type(series) ~= "table" or #series < 2 then return nil end
  local first = series[1]
  local lead0 = first.rad - first.room
  if lead0 <= 0 then return nil end
  local target = lead0 / 2
  local prev = first
  for index = 2, #series do
    local point = series[index]
    local lead = point.rad - point.room
    if lead <= target then
      local prev_lead = prev.rad - prev.room
      local span = prev_lead - lead
      if span <= 0 then return point.t - first.t end
      local fraction = (prev_lead - target) / span
      return (prev.t + (point.t - prev.t) * fraction) - first.t
    end
    prev = point
  end
  return nil
end

-- Whether an episode may be learned from, plus why not. The reason is returned
-- rather than a bare boolean so the journal, the log line and the tests all
-- assert on the same string instead of re-deriving it.
function M.valid(episode)
  if episode.invalid ~= nil then return false, episode.invalid end
  if episode.cutoff_at == nil then return false, "never_reached" end
  if episode.heated == false then return false, "never_heated" end
  if episode.lead_at_cutoff == nil then return false, "no_radiator" end
  if episode.lead_at_cutoff < M.MIN_LEAD then return false, "radiator_cold" end
  return true, nil
end

-- Folds a finished episode into c. Returns the new c, the outcome
-- ("learned"/"observed"/"discarded") and a discard reason.
--
-- A measurement, smoothed — not an integrator on the error. Whoever cut the run,
-- the room rose (peak - room at the cutoff) on (radiator - room at the cutoff)
-- of stored heat, and that ratio is what the next prediction needs. So
-- observe-only, which only ever sees the node's own cutoff, converges on the
-- same physical number instead of drifting on an error nothing it does can
-- change. "observed" names that regime apart for a reader.
function M.close(episode, c)
  local ok, reason = M.valid(episode)
  if not ok then return c, "discarded", reason end
  local observed = (episode.peak - episode.room_at_cutoff) / episode.lead_at_cutoff
  episode.c_observed = observed
  local next_c = clamp(c + M.GAIN * (observed - c), 0, M.C_MAX)
  return next_c, episode.observe_only and "observed" or "learned", nil
end

-- Flattens a closed episode into the journal row of spec §9.1. Deciding inputs
-- and resulting action are both kept so an episode can be re-judged months later
-- without the surrounding state.
function M.record(episode, zone, c_before, c_after, outcome, reason, closed_at)
  return {
    zone = zone,
    opened_at = episode.opened_at,
    closed_at = closed_at,
    requested = episode.requested,
    current_at_open = episode.current_at_open,
    rise = episode.rise,
    observe_only = episode.observe_only,
    outdoor_at_open = episode.outdoor_at_open,
    radiator_at_open = episode.radiator_at_open,
    radiator_min = episode.radiator_min,
    gate_at = episode.gate_at,
    would_cut_at = episode.would_cut_at,
    would_cut_predicted = episode.would_cut_predicted,
    would_cut_lead = episode.would_cut_lead,
    cutoff_at = episode.cutoff_at,
    cut_by = episode.cut_by,
    room_at_cutoff = episode.room_at_cutoff,
    radiator_at_cutoff = episode.radiator_at_cutoff,
    lead_at_cutoff = episode.lead_at_cutoff,
    predicted_at_cutoff = episode.predicted_at_cutoff,
    hold_temp = episode.hold_temp,
    released_at = episode.released_at,
    peak = episode.peak,
    peak_at = episode.peak_at,
    radiator_at_peak = episode.radiator_at_peak,
    error = episode.peak - episode.requested,
    decay = episode.decay,
    decay_half_life = M.half_life(episode),
    heated = episode.heated,
    c_used = episode.c_used,
    c_observed = episode.c_observed,
    c_before = c_before,
    c_after = c_after,
    outcome = outcome,
    reason = reason,
  }
end

return M
