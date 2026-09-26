-- lib/overshoot.lua
--
-- Pure heating-overshoot math, free of any ha.* / store.* / time access so it
-- can be unit-tested directly from Go. The controller does the clock and I/O
-- work, keeps the episode table this returns, and feeds it back every tick.
--
-- The problem (overshoot-spec.md): a bang-bang thermostat drives the relay full
-- on until the room reaches setpoint, by which point the actuator still takes
-- minutes to close and the radiator body keeps heating for another quarter of an
-- hour. A small room has no mass to absorb that, so it sails past.
--
-- The fix is to stop earlier: command `requested - k * rise` for the whole
-- warmup and let the stored energy land the room on target. `k` is learned from
-- each episode's measured peak, so it tracks seasonal drift on its own. Scaling
-- by the rise is what lets one scalar serve both a 3° warmup from setback and a
-- 0.2° top-up, which then gets essentially no correction.

local M = {}

-- Zero: having learned nothing, the correction must behave exactly as the
-- uncorrected controller did.
M.K_INIT = 0
-- Fraction of each episode's error folded into k: converges in about four
-- episodes, damped enough not to ring.
M.GAIN = 0.5
M.K_MAX = 0.8
-- Absolute ceiling on one cutoff, whatever k * rise says.
M.MAX_OFFSET = 2.5
-- Below this a rise teaches nothing, the overshoot being sensor noise. The
-- correction still applies; only the learning is skipped.
M.MIN_RISE = 0.3
-- How long past the cutoff the peak is watched for; the coast is a broad hump.
M.COAST_SECONDS = 30 * 60
-- An episode that has not reached its setpoint in this long is abandoned, so a
-- room the heating cannot satisfy fails loudly instead of staying open forever.
M.MAX_EPISODE_SECONDS = 4 * 3600
-- Ceiling on the coast decay series. A 30-minute coast on a 1-minute tick fills
-- about 30 slots; the cap is what stops a faster tick from growing the journal
-- row without bound.
M.DECAY_MAX_SAMPLES = 40

local function clamp(value, lo, hi)
  if value < lo then return lo end
  if value > hi then return hi end
  return value
end

-- offset returns how far below the request to stop, for a coefficient and the
-- rise being attempted. Clamped to [0, MAX_OFFSET].
function M.offset(k, rise)
  return clamp(k * rise, 0, M.MAX_OFFSET)
end

-- Starts an episode, or nil when the request is not above the room (nothing to
-- climb, nothing to overshoot). `at` is epoch seconds.
--
-- The offset is computed ONCE and held for the whole episode. Recomputed per
-- tick it would climb as the room warmed and the rise shrank, converging on the
-- request without ever cutting early — the correction would silently do nothing.
-- This is the easiest thing in the design to get wrong.
--
-- `commanded` is what the correction wants written; `applied` is what the caller
-- will write, which in observe-only mode is the uncorrected request, so the
-- episode measures the uncorrected run that k needs to converge on.
--
-- `env` is an optional {outdoor =, radiator =} snapshot recorded alongside the
-- episode. Nothing here reads it — it exists so the journal can later be tested
-- for the correlations this single scalar deliberately does NOT model (a mild
-- day against a cold one, a hot radiator against a lukewarm one). Recording it
-- is free; not recording it makes the question permanently unanswerable.
function M.open(requested, current, k, observe_only, at, env)
  local rise = requested - current
  if rise <= 0 then return nil end
  local offset = M.offset(k, rise)
  local episode = {
    opened_at = at,
    requested = requested,
    current_at_open = current,
    rise = rise,
    k_used = k,
    offset = offset,
    commanded = requested - offset,
    applied = observe_only and requested or requested - offset,
    observe_only = observe_only and true or false,
    peak = current,
    peak_at = at,
  }
  if env ~= nil then
    episode.outdoor_at_open = env.outdoor
    episode.radiator_at_open = env.radiator
  end
  return episode
end

-- Marks an episode unusable for learning, with one of "window_open",
-- "mode_left_heat", "setpoint_changed", "restart", "never_reached" or
-- "observe_changed" ("rise_too_small" is derived by valid() instead).
--
-- The FIRST reason wins: the one a reader wants is what broke the episode, not
-- what happened to it afterwards.
function M.invalidate(episode, reason)
  if episode.invalid == nil then episode.invalid = reason end
  return episode
end

-- Advances an episode by one observation and reports its phase: "heating" (still
-- climbing to the cutoff), "coasting" (cut off, watching the peak) or "done".
--
-- `env` is the optional snapshot of M.open. The radiator temperature AT THE
-- CUTOFF is the interesting one: it is the stored energy about to be dumped into
-- the room, which is the thing that actually causes the overshoot. The one at
-- the peak says how much of it was still left when the room stopped rising.
function M.step(episode, current, at, env)
  local radiator = env and env.radiator or nil
  if current > episode.peak then
    episode.peak, episode.peak_at = current, at
    episode.radiator_at_peak = radiator
  end
  if episode.cutoff_at == nil then
    if current >= episode.applied then
      episode.cutoff_at = at
      episode.radiator_at_cutoff = radiator
      M.sample_decay(episode, current, at, radiator)
      return "coasting"
    end
    if at - episode.opened_at >= M.MAX_EPISODE_SECONDS then
      M.invalidate(episode, "never_reached")
      return "done"
    end
    return "heating"
  end
  M.sample_decay(episode, current, at, radiator)
  if at - episode.cutoff_at >= M.COAST_SECONDS then return "done" end
  return "coasting"
end

-- Appends one point of the coast decay curve: seconds since the cutoff, the
-- radiator, and the room it is emptying into.
--
-- The cool-down is not a correlate of the overshoot, it IS the overshoot — the
-- heat that lands in the room after the relay drops is the integral of this
-- curve. Two endpoints cannot tell an exponential from a straight line, which
-- is why the middle is kept rather than just a start and an end.
--
-- Recorded, never acted on. The correction stays outcome-based: it measures the
-- peak it actually got. This is here to explain a coefficient, and to make a
-- plant that has CHANGED visible — a decay that suddenly shortens is air in the
-- radiator or a valve that stopped closing, which no peak measurement shows.
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
-- window closed — "did not halve in 30 minutes" is itself worth seeing.
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
  if episode.rise < M.MIN_RISE then return false, "rise_too_small" end
  return true, nil
end

-- Folds a finished episode into the coefficient. Returns the new k, the outcome
-- ("learned"/"observed"/"discarded") and a discard reason.
--
-- The update is a discrete integral controller closed across days: the error is
-- how far the peak landed above the request, normalised by the rise so a small
-- top-up cannot swing k as hard as a full warmup. "observed" is a real learned
-- update from an uncorrected run, named apart so a reader can tell which regime
-- a sample came from.
function M.close(episode, k)
  local ok, reason = M.valid(episode)
  if not ok then return k, "discarded", reason end
  local err = episode.peak - episode.requested
  -- The lower clamp is defensive: a run cuts off at requested - k*rise, so the
  -- update is bounded below by -GAIN*k and k halves toward zero without crossing.
  local next_k = clamp(k + M.GAIN * err / math.max(episode.rise, M.MIN_RISE), 0, M.K_MAX)
  return next_k, episode.observe_only and "observed" or "learned", nil
end

-- Flattens a closed episode into the journal row of spec §9.1. Deciding inputs
-- and resulting action are both kept so an episode can be re-judged months later
-- without the surrounding state.
function M.record(episode, zone, k_before, k_after, outcome, reason, closed_at)
  return {
    zone = zone,
    opened_at = episode.opened_at,
    closed_at = closed_at,
    requested = episode.requested,
    current_at_open = episode.current_at_open,
    rise = episode.rise,
    k_used = episode.k_used,
    offset = episode.offset,
    commanded = episode.commanded,
    applied = episode.applied,
    observe_only = episode.observe_only,
    cutoff_at = episode.cutoff_at,
    peak = episode.peak,
    peak_at = episode.peak_at,
    outdoor_at_open = episode.outdoor_at_open,
    radiator_at_open = episode.radiator_at_open,
    radiator_at_cutoff = episode.radiator_at_cutoff,
    radiator_at_peak = episode.radiator_at_peak,
    decay = episode.decay,
    decay_half_life = M.half_life(episode),
    error = episode.peak - episode.requested,
    k_before = k_before,
    k_after = k_after,
    outcome = outcome,
    reason = reason,
  }
end

return M
