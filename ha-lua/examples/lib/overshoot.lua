-- lib/overshoot.lua
--
-- Pure heating-overshoot math, deliberately free of any ha.* / store.* / time
-- access so it can be unit-tested directly from Go (like lib/schedule.lua and
-- lib/control.lua). The controller does the clock and I/O work, keeps the
-- episode table this module returns, and feeds it back in every tick.
--
-- The problem (overshoot-spec.md): a bang-bang thermostat drives the relay
-- full on until the room reaches setpoint, by which point the actuator still
-- takes minutes to close and the radiator body is hot enough to keep heating
-- the room for another quarter of an hour. A small room has no mass to absorb
-- that, so it sails past. Nothing on the device can anticipate it.
--
-- The fix is to stop earlier: command `requested - k * rise` for the whole
-- warmup and let the stored energy land the room on target instead of above
-- it. `k` is learned from the measured peak of each episode, so it tracks
-- seasonal drift on its own. Proportional to the rise is what lets one scalar
-- serve both a 3° warmup from setback and a 0.2° top-up (which then gets
-- essentially no correction, leaving the steady-state hold band alone).

local M = {}

-- K_INIT is deliberately zero: having learned nothing, the correction must
-- behave exactly as the uncorrected controller did. Never worse than the
-- status quo on day one.
M.K_INIT = 0
-- Fraction of each episode's measured error folded into k. Converges in about
-- four episodes and is damped enough not to ring.
M.GAIN = 0.5
M.K_MAX = 0.8
-- Absolute ceiling on one cutoff, whatever k * rise says.
M.MAX_OFFSET = 2.5
-- Below this a rise teaches nothing — the overshoot is sensor noise. The
-- correction still applies; only the learning is skipped.
M.MIN_RISE = 0.3
-- How long past the cutoff the peak is watched for. The coast is a broad hump,
-- so minute sampling is ample.
M.COAST_SECONDS = 30 * 60
-- An episode that has not reached its setpoint in this long is abandoned. It
-- exists so a room the heating cannot satisfy fails loudly instead of leaving
-- an episode open forever, learning nothing and saying nothing.
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

-- open starts an episode, or returns nil when the request is not above the
-- room (nothing to climb, so nothing to overshoot). `at` is epoch seconds.
--
-- The offset is computed ONCE, here, and held for the whole episode. Recomputed
-- per tick it would climb as the room warmed and the rise shrank, converging on
-- the request without ever cutting early — the correction would silently do
-- nothing. This is the single easiest thing to get wrong in the design.
--
-- `commanded` is what the correction wants written; `applied` is what the
-- caller will actually write, which in observe-only mode is the uncorrected
-- request. The episode then measures a completely uncorrected run, which is
-- exactly the data k needs to converge before the correction is trusted.
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

-- invalidate marks an episode unusable for learning. The FIRST reason wins: a
-- window opened during a warmup that later also left heat mode is a
-- window_open episode, and the reason a reader wants is the one that broke it.
function M.invalidate(episode, reason)
  if episode.invalid == nil then episode.invalid = reason end
  return episode
end

-- step advances an episode by one observation and reports its phase:
-- "heating" (still climbing to the cutoff), "coasting" (cut off, watching the
-- peak) or "done" (the caller should now close it).
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

-- valid reports whether an episode may be learned from, and why not when it may
-- not. It returns the reason rather than a bare boolean deliberately: the
-- journal, the log line and the tests then all assert on the same string
-- instead of the caller re-deriving it and the two copies drifting apart.
function M.valid(episode)
  if episode.invalid ~= nil then return false, episode.invalid end
  if episode.cutoff_at == nil then return false, "never_reached" end
  if episode.rise < M.MIN_RISE then return false, "rise_too_small" end
  return true, nil
end

-- close folds a finished episode into the coefficient. Returns the new k, the
-- outcome ("learned"/"observed"/"discarded") and the discard reason or nil.
--
-- The update is a discrete integral controller closed across days: the error is
-- how far the peak landed above what was asked for, normalised by the rise so a
-- small top-up cannot swing k as hard as a full warmup. "observed" is a real
-- learned update from a run where nothing was corrected — the distinction is
-- kept so a reader can tell which regime a sample came from.
function M.close(episode, k)
  local ok, reason = M.valid(episode)
  if not ok then return k, "discarded", reason end
  local err = episode.peak - episode.requested
  -- The lower clamp is defensive only. A run cuts off at requested - k*rise, so
  -- the peak cannot land more than the offset below the request and the update
  -- is bounded below by -GAIN*k: k halves toward zero without ever crossing it.
  local next_k = clamp(k + M.GAIN * err / math.max(episode.rise, M.MIN_RISE), 0, M.K_MAX)
  return next_k, episode.observe_only and "observed" or "learned", nil
end

-- record flattens a closed episode into the journal row of spec §9.1: what it
-- decided, what it decided that from, what actually happened, and what it
-- concluded. Deciding inputs and resulting action are both kept so an episode
-- can be re-judged months later without the surrounding state.
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
