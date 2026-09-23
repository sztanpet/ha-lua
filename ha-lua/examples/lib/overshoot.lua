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
function M.open(requested, current, k, observe_only, at)
  local rise = requested - current
  if rise <= 0 then return nil end
  local offset = M.offset(k, rise)
  return {
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
function M.step(episode, current, at)
  if current > episode.peak then
    episode.peak, episode.peak_at = current, at
  end
  if episode.cutoff_at == nil then
    if current >= episode.applied then
      episode.cutoff_at = at
      return "coasting"
    end
    if at - episode.opened_at >= M.MAX_EPISODE_SECONDS then
      M.invalidate(episode, "never_reached")
      return "done"
    end
    return "heating"
  end
  if at - episode.cutoff_at >= M.COAST_SECONDS then return "done" end
  return "coasting"
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
    error = episode.peak - episode.requested,
    k_before = k_before,
    k_after = k_after,
    outcome = outcome,
    reason = reason,
  }
end

return M
