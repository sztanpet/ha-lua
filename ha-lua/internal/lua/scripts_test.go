package lua

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// repoScriptsDir is the shipped example/script tree, relative to this package.
const repoScriptsDir = "../../examples"

// TestShippedScriptsCompile loads every *.lua under examples/ (compile only, no
// execution) so a syntax error in a shipped script is caught by `make test`
// rather than at runtime inside the daemon. ha.*/store.*/require references are
// fine here: LoadFile compiles but never runs the chunk.
func TestShippedScriptsCompile(t *testing.T) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()

	var found int
	err := filepath.Walk(repoScriptsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".lua") {
			return nil
		}
		found++
		if _, lerr := L.LoadFile(path); lerr != nil {
			t.Errorf("%s: %v", path, lerr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("no scripts found to compile")
	}
}

// newScheduleState boots an LState whose require resolves into the repo's
// examples/lib, so tests exercise the actual shipped lib/schedule.lua.
func newScheduleState(t *testing.T) *lua.LState {
	t.Helper()
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	L.SetContext(context.Background())
	t.Cleanup(L.Close)
	RegisterStdlib(L, repoScriptsDir, openTestRoot(t, repoScriptsDir))
	return L
}

func TestSchedulePureLib(t *testing.T) {
	L := newScheduleState(t)

	// Drive the pure functions from Lua and let assert() surface failures as a
	// DoString error with a useful message.
	err := L.DoString(`
		local s = require "schedule"

		-- parse_hhmm
		assert(s.parse_hhmm("06:30") == 390, "parse 06:30")
		assert(s.parse_hhmm("00:00") == 0, "parse 00:00")
		assert(s.parse_hhmm("23:59") == 1439, "parse 23:59")
		assert(s.parse_hhmm("24:00") == nil, "reject 24:00")
		assert(s.parse_hhmm("6:30") == nil, "reject single-digit hour")
		assert(s.parse_hhmm("ab:cd") == nil, "reject non-numeric")

		local days = { ["0"] = {
			{time="06:30", temp=21}, {time="08:00", temp=18},
			{time="17:00", temp=21}, {time="22:00", temp=16},
		} }

		-- Mid-day: 09:00 -> the 08:00 step (idx 1), next is 17:00 (480 min away).
		local t, idx, nxt = s.resolve(days, 0, 9*60)
		assert(t == 18, "midday temp "..tostring(t))
		assert(idx == 1, "midday idx "..tostring(idx))
		assert(nxt == 480, "midday next "..tostring(nxt))

		-- Late night: 23:00 -> last step today (idx 3); next wraps to Tuesday.
		days["1"] = { {time="06:00", temp=20} }
		t, idx, nxt = s.resolve(days, 0, 23*60)
		assert(t == 16, "night temp "..tostring(t))
		assert(idx == 3, "night idx "..tostring(idx))
		assert(nxt == 1440 - 23*60 + 360, "night next "..tostring(nxt))

		-- Carryover before the first transition: Sunday's last step carries into
		-- early Monday, idx -1, next is Monday 06:30.
		local cd = {
			["6"] = { {time="22:00", temp=15} },
			["0"] = { {time="06:30", temp=21} },
		}
		t, idx, nxt = s.resolve(cd, 0, 5*60)
		assert(t == 15, "carry temp "..tostring(t))
		assert(idx == -1, "carry idx "..tostring(idx))
		assert(nxt == 90, "carry next "..tostring(nxt))

		-- Empty schedule: nil everywhere.
		t, idx, nxt = s.resolve({}, 0, 600)
		assert(t == nil and nxt == nil, "empty schedule")

		-- validate
		assert(s.validate(days) == true, "valid days")
		local ok, msg = s.validate({ ["0"] = { {time="99:99", temp=20} } })
		assert(ok == false and msg ~= nil, "bad time rejected")
		ok, msg = s.validate({ ["0"] = { {time="06:00", temp=99} } })
		assert(ok == false and msg ~= nil, "out-of-range temp rejected")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestControlPureLib(t *testing.T) {
	L := newScheduleState(t)

	err := L.DoString(`
		local c = require "control"

		-- desired: override > manual > schedule, nil when none.
		local t, src = c.desired(22, 20, 18)
		assert(t == 22 and src == "override", "override wins")
		t, src = c.desired(nil, 20, 18)
		assert(t == 20 and src == "manual", "manual beats schedule")
		t, src = c.desired(nil, nil, 18)
		assert(t == 18 and src == "schedule", "schedule fallback")
		t, src = c.desired(nil, nil, nil)
		assert(t == nil and src == nil, "no source -> nil")

		-- is_manual: a device-rounded write is ours, one dial step is not.
		assert(c.is_manual(21.0, 21) == false, "21 vs 21.0 not manual")
		assert(c.is_manual(21.05, 21) == false, "within tolerance not manual")
		assert(c.is_manual(21.2, 21) == true, "beyond tolerance is manual")
		-- Float error puts these one-step pairs a hair under 0.1.
		assert(c.is_manual(23.3, 23.4) == true, "23.4 -> 23.3 is manual")
		assert(c.is_manual(22.9, 22.8) == true, "22.8 -> 22.9 is manual")
		assert(c.is_manual(23.6, 23.7) == true, "23.7 -> 23.6 is manual")
		assert(c.is_manual(23.4, 23.5) == true, "23.5 -> 23.4 is manual")
		assert(c.is_manual(21, nil) == true, "never-published is manual")

		-- should_write: heat mode, no window, value changed > 0.05.
		assert(c.should_write("heat", false, 19.5, 21) == true, "changed -> write")
		assert(c.should_write("heat", false, nil, 21) == true, "no current -> write")
		assert(c.should_write("heat", false, 21.0, 21.02) == false, "unchanged -> skip")
		assert(c.should_write("off", false, 19, 21) == false, "not heat -> skip")
		assert(c.should_write("heat", true, 19, 21) == false, "window open -> skip")

		-- clamp_bounds: device min/max.
		assert(c.clamp_bounds(40, 5, 30) == 30, "clamp high")
		assert(c.clamp_bounds(2, 5, 30) == 5, "clamp low")
		assert(c.clamp_bounds(21, 5, 30) == 21, "in range")

		-- window_open: any open / all closed.
		assert(c.window_open({"off", "on", "off"}) == true, "any open")
		assert(c.window_open({"off", "off"}) == false, "all closed")
		assert(c.window_open({}) == false, "no sensors -> closed")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOvershootPureLib(t *testing.T) {
	L := newScheduleState(t)

	err := L.DoString(`
		local o = require "overshoot"
		local near = function(a, b) return math.abs(a - b) < 1e-9 end
		local E = function(radiator, heating) return { radiator = radiator, heating = heating } end

		-- predict: where the room ends up if the heat stops now.
		assert(o.predict(0, 23.3, 45) == 23.3, "c=0: the room itself")
		assert(near(o.predict(0.02, 23.3, 33.3), 23.5), "room + c * lead")
		assert(o.predict(0.02, 23.3, 20) == 23.3, "a radiator below the room adds nothing")
		assert(o.predict(0.02, 23.3, nil) == nil, "no radiator, no prediction")

		-- open decides nothing: the correction acts only on evidence from the run.
		assert(o.open(23.4, 23.5, 0, false, 0) == nil, "room above the request: nothing to climb")
		local opened = o.open(23.4, 23.3, 0.02, false, 1000, { radiator = 23.3, heating = true, outdoor = 17 })
		assert(opened.hold == nil and opened.cutoff_at == nil, "opening a run writes nothing")
		assert(opened.radiator_min == 23.3 and opened.heated == true and opened.outdoor_at_open == 17, "open snapshot")

		-- Rule 2: never a cut before the radiator is SEEN warming in this run — not
		-- even with a c that says the stored heat already suffices. A radiator still
		-- warm from the last run is not evidence about this one.
		local eager = o.open(23.4, 23.3, 0.2, false, 0, E(30, true))
		assert(o.step(eager, 23.3, 60, E(29, true)) == "heating", "radiator still cooling: no cut")
		assert(eager.radiator_min == 29 and eager.gate_at == nil, "the minimum follows it down")
		assert(o.step(eager, 23.3, 120, E(29.8, true)) == "heating", "not yet RAD_RISE above its minimum")
		assert(o.step(eager, 23.3, 180, E(30.0, true)) == "coasting", "seen warming: now it may cut")
		assert(eager.gate_at == 180 and eager.cut_by == "overshoot", "cut on evidence")

		-- Without a radiator reading there is never any evidence, so never a cut.
		local blind = o.open(23.4, 23.3, 0.2, false, 0, { heating = true })
		assert(o.step(blind, 23.3, 60, { heating = true }) == "heating" and blind.hold == nil, "no radiator: never cut")

		-- The cut comes when room + c * lead reaches the request, not before.
		local run = o.open(23.4, 23.3, 0.02, false, 0, E(23.3, true))
		assert(o.step(run, 23.3, 60, E(23.2, true)) == "heating", "valve still opening")
		assert(o.step(run, 23.3, 120, E(25.0, true)) == "heating", "warming, predicted 23.336")
		assert(run.gate_at == 120, "gate at the first reading RAD_RISE above the minimum")
		assert(o.step(run, 23.3, 180, E(28.2, true)) == "heating", "predicted 23.398: a hair short")
		assert(o.step(run, 23.3, 240, E(28.4, true)) == "coasting", "predicted 23.402: cut")
		assert(run.cutoff_at == 240 and run.hold == true, "an armed cut holds the run off")
		assert(near(run.hold_temp, 23.3 - o.HOLD_MARGIN), "the hold sits just below the room")
		assert(near(run.lead_at_cutoff, 28.4 - 23.3), "lead recorded")
		assert(near(run.predicted_at_cutoff, 23.3 + 0.02 * (28.4 - 23.3)), "prediction recorded")
		assert(run.peak == 23.3 and run.peak_at == 240, "the coast's peak starts at the cutoff")
		-- The hold lasts while the stored heat can still reach the request...
		assert(o.step(run, 23.3, 300, E(29.0, false)) == "coasting" and run.hold, "actuator lag: still climbing")
		assert(o.step(run, 23.5, 900, E(27.0, false)) == "coasting" and run.hold, "room rising on stored heat")
		-- ...and goes with the coast when the room turns.
		assert(o.step(run, 23.3, 900 + o.PEAK_HOLD_SECONDS, E(25.5, false)) == "done", "turned")

		-- Rule 1: a cut that was too early gives the run back the moment the stored
		-- heat can no longer carry the room to the request.
		local early = o.open(23.4, 23.3, 0.2, false, 0, E(23.0, true))
		o.step(early, 23.3, 60, E(24.1, true))
		assert(early.hold == true, "cut at the gate: predicted 23.46")
		assert(o.step(early, 23.3, 120, E(23.6, false)) == "coasting", "")
		assert(early.hold == false and early.released_at == 120, "predicted 23.36 < 23.4: released")
		-- The node firing again is the next run, not part of this one.
		assert(o.step(early, 23.3, 180, E(23.7, true)) == "done", "relay closed again: the next run")

		-- After an armed cut the node may still report heating until its minimum
		-- run time is up; that is not the next run.
		local lag = o.open(23.4, 23.3, 0.2, false, 0, E(23.3, true))
		o.step(lag, 23.3, 60, E(24.5, true))
		assert(lag.cut_by == "overshoot", "cut")
		assert(o.step(lag, 23.3, 90, E(25, true)) == "coasting", "still reported heating: not a new run")
		assert(o.step(lag, 23.3, 150, E(25.5, false)) == "coasting", "relay opened")
		assert(o.step(lag, 23.3, 210, E(25.4, true)) == "done", "relay closed again: the next run")

		-- c = 0 never cuts before the node would: the prediction is the room.
		local zero = o.open(23.4, 23.3, 0, false, 0, E(23.3, true))
		assert(o.step(zero, 23.3, 60, E(40, true)) == "heating", "c=0: a hot radiator alone is no reason")
		assert(o.step(zero, 23.4, 120, E(45, true)) == "coasting" and zero.cut_by == "overshoot", "c=0 cuts at the request")

		-- Observe-only records when it would have cut, never holds, and its cutoff is
		-- always the node's own.
		local watch = o.open(23.4, 23.3, 0.02, true, 0, E(23.3, true))
		o.step(watch, 23.3, 60, E(25.0, true))
		assert(o.step(watch, 23.3, 120, E(28.4, true)) == "heating", "observe-only keeps heating")
		assert(watch.would_cut_at == 120 and watch.hold == nil, "would have cut; did not")
		assert(near(watch.would_cut_lead, 28.4 - 23.3), "would-cut lead")
		o.step(watch, 23.3, 180, E(35, true))
		assert(watch.would_cut_at == 120, "the first would-cut is the one kept")
		assert(o.step(watch, 23.5, 600, E(45.5, false)) == "coasting", "the node's own cutoff")
		assert(watch.cut_by == "device" and near(watch.lead_at_cutoff, 45.5 - 23.5), "measured like the correction's")

		-- A device that reports no relay: its cutoff is the room passing the request.
		local mute = o.open(23.4, 23.3, 0.02, true, 0, { radiator = 23.3 })
		assert(o.step(mute, 23.4, 60, { radiator = 30 }) == "heating", "at the request is not past it")
		assert(o.step(mute, 23.5, 120, { radiator = 40 }) == "coasting" and mute.cut_by == "device", "past it")

		-- The coast's peak starts at the cutoff: a flicker during the run must not
		-- read as the room having turned.
		local flicker = o.open(23.4, 23.3, 0, true, 0, E(23.3, true))
		o.step(flicker, 23.5, 60, E(30, true))
		o.step(flicker, 23.3, 120, E(35, false))
		assert(flicker.peak == 23.3, "peak restarted at the cutoff")
		assert(o.step(flicker, 23.3, 120 + o.PEAK_HOLD_SECONDS + 60, E(33, false)) == "coasting", "no turn off a run-time reading")

		-- close measures c, it does not integrate an error. The first live run: the
		-- node cut with the radiator 21.77° above the room, and the room coasted 0.4°.
		local live = o.open(23.7, 23.5, 0, true, 0, E(23.5, true))
		o.step(live, 23.5, 60, E(30, true))
		o.step(live, 23.7, 810, E(45.47, false))
		o.step(live, 24.1, 1710, E(39.42, false))
		local c_after, outcome, reason = o.close(live, 0)
		assert(outcome == "observed" and reason == nil, "observe-only learns")
		assert(near(live.c_observed, (24.1 - 23.7) / (45.47 - 23.7)), "c observed = coast / lead")
		assert(near(c_after, o.GAIN * live.c_observed), "smoothed toward it")

		-- Watching converges on the measured c. The v4.13 learner, fed the same
		-- uncorrected runs, climbed without bound: an integrator on an open loop.
		local c = 0
		for i = 1, 20 do
			local r = o.open(23.7, 23.5, c, true, 0, E(23.5, true))
			o.step(r, 23.5, 60, E(30, true))
			o.step(r, 23.7, 810, E(45.47, false))
			o.step(r, 24.1, 1710, E(39.42, false))
			c = o.close(r, c)
		end
		assert(math.abs(c - (24.1 - 23.7) / (45.47 - 23.7)) < 1e-4, "watching converges, got "..tostring(c))

		-- c stays inside its bounds however wild the run.
		local wild = o.open(23.4, 23.3, 0.19, false, 0, E(23.3, true))
		o.step(wild, 23.3, 60, E(26.5, true))
		o.step(wild, 25.3, 600, E(26, false))
		assert(o.close(wild, 0.19) == o.C_MAX, "clamped to C_MAX")

		-- Discards carry the reason, and the FIRST reason wins (spec §9.2).
		local windowed = o.open(23.4, 23.3, 0.02, false, 0, E(23.3, true))
		o.invalidate(windowed, "window_open")
		o.invalidate(windowed, "mode_left_heat")
		local c_same, outcome2, reason2 = o.close(windowed, 0.02)
		assert(c_same == 0.02 and outcome2 == "discarded" and reason2 == "window_open", "first reason wins, c untouched")

		local unfired = o.open(23.4, 23.3, 0.2, false, 0, E(23.3, false))
		o.step(unfired, 23.3, 60, E(27, false))
		o.step(unfired, 23.3, 60 + o.MAX_COAST_SECONDS, E(24, false))
		local _, _, why_unfired = o.close(unfired, 0.2)
		assert(why_unfired == "never_heated", "relay never closed, got "..tostring(why_unfired))

		local no_rad = o.open(23.4, 23.3, 0.02, false, 0, {})
		assert(o.step(no_rad, 23.5, 60, {}) == "coasting" and no_rad.cut_by == "device", "")
		o.step(no_rad, 23.5, 60 + o.MAX_COAST_SECONDS, {})
		local _, _, why_no_rad = o.close(no_rad, 0.02)
		assert(why_no_rad == "no_radiator", "no radiator, got "..tostring(why_no_rad))

		local cold = o.open(23.4, 23.3, 0, false, 0, E(23.3, true))
		o.step(cold, 23.3, 60, E(25.0, true))
		assert(o.step(cold, 23.5, 120, E(25.5, false)) == "coasting" and cold.cut_by == "device", "")
		o.step(cold, 23.5, 120 + o.MAX_COAST_SECONDS, E(24, false))
		local _, _, why_cold = o.close(cold, 0)
		assert(why_cold == "radiator_cold", "lead 2.0 < MIN_LEAD, got "..tostring(why_cold))

		local stuck = o.open(23.4, 23.3, 0, false, 0, E(23.3, true))
		assert(o.step(stuck, 23.2, 3600, E(23.3, true)) == "heating", "still trying")
		assert(o.step(stuck, 23.2, o.MAX_EPISODE_SECONDS, E(23.3, true)) == "done", "given up")
		local ok_stuck, why_stuck = o.valid(stuck)
		assert(ok_stuck == false and why_stuck == "never_reached", "never_reached, got "..tostring(why_stuck))

		-- The coast ends when the room TURNS: a full step below its peak, with the
		-- peak settled for PEAK_HOLD_SECONDS.
		local turn = o.open(21, 20.8, 0, false, 0, E(20.8, true))
		o.step(turn, 21.1, 600, E(45, false))
		o.step(turn, 21.4, 1500, E(40, false))
		assert(o.step(turn, 21.3, 1560, E(39, false)) == "coasting", "0.1 below is inside noise")
		assert(o.step(turn, 21.2, 1620, E(38, false)) == "coasting", "a full step, but the peak is 2 min old")
		assert(o.step(turn, 21.2, 1500 + o.PEAK_HOLD_SECONDS, E(37, false)) == "done", "turned")
		assert(turn.peak == 21.4 and turn.peak_at == 1500, "the peak is the turn's")
		local exact = o.open(16.1, 15.9, 0, false, 0, E(15.9, true))
		o.step(exact, 16.08, 60, E(40, false))
		assert(exact.peak == 16.08, "peak is 16.08")
		assert(o.step(exact, 15.88, 60 + o.PEAK_HOLD_SECONDS, E(35, false)) == "done", "16.08 -> 15.88 is a turn")
		local flat = o.open(21, 20.8, 0, false, 0, E(20.8, true))
		o.step(flat, 21.1, 600, E(45, false))
		assert(o.step(flat, 21.4, 600 + o.MAX_COAST_SECONDS - 60, E(40, false)) == "coasting", "before the backstop")
		assert(o.step(flat, 21.4, 600 + o.MAX_COAST_SECONDS, E(40, false)) == "done", "backstop")

		-- The coast decay series: the cool-down IS the overshoot, so the middle of
		-- the curve is kept and not just its endpoints.
		local decay = o.open(21, 20.8, 0, false, 0, E(20, true))
		o.step(decay, 20.9, 60, E(55, true))
		assert(decay.decay == nil, "no decay samples before the cutoff")
		o.step(decay, 21, 120, E(60, false))
		assert(#decay.decay == 1 and decay.decay[1].t == 0, "cutoff is the first sample")
		assert(decay.decay[1].rad == 60 and decay.decay[1].room == 21, "sample carries both")
		o.step(decay, 21.5, 300, E(45, false))
		o.step(decay, 21.5, 600, E(32, false))
		assert(#decay.decay == 3, "coast samples appended")
		local half = o.half_life(decay)
		assert(half ~= nil and half > 180 and half < 480, "half-life interpolated, got "..tostring(half))
		local slow = o.open(21, 20.8, 0, false, 0, E(20, true))
		o.step(slow, 21, 60, E(60, false))
		o.step(slow, 21, 120, E(59, false))
		assert(o.half_life(slow) == nil, "no halving -> nil")
		local many = o.open(21, 20.8, 0, false, 0, E(20, true))
		o.step(many, 21, 1, E(60, false))
		for i = 2, o.DECAY_MAX_SAMPLES + 20 do
			o.step(many, 21, i, E(60 - i * 0.1, false))
		end
		assert(#many.decay == o.DECAY_MAX_SAMPLES, "decay series capped, got "..tostring(#many.decay))
		local gap = o.open(21, 20.8, 0, false, 0, E(20, true))
		o.step(gap, 21, 60, E(60, false))
		o.step(gap, 21, 120, { heating = false })
		assert(#gap.decay == 1, "a nil radiator adds no sample")

		-- record carries the decision, the evidence and the outcome.
		local row = o.record(live, "childrens", 0, c_after, "observed", nil, 9999)
		assert(row.zone == "childrens" and row.closed_at == 9999, "record identity")
		assert(row.cut_by == "device" and near(row.lead_at_cutoff, 45.47 - 23.7), "record cutoff")
		assert(row.c_before == 0 and row.c_after == c_after and near(row.c_observed, live.c_observed), "record c")
		assert(row.heated == true and row.outcome == "observed" and row.reason == nil, "record outcome")
		assert(near(row.error, 24.1 - 23.7) and #row.decay == 2, "record coast")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

// openTestRoot opens an os.Root over dir, closed on test cleanup. It backs
// the fs module, require, and Supervisor.LoadAll's script enumeration.
func openTestRoot(t testing.TB, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func copyRepoFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// testZonesLua is a fixture mirroring the structure of examples/lib/zones.lua
// with stable entity ids the thermostat tests seed. The shipped zones.lua holds
// the maintainer's real entities and is meant to be user-edited, so the tests
// must not depend on its contents — they write this fixture instead.
const testZonesLua = `local M = {}
M.frost_temp = 15
M.default_override_temp = 21
M.zones = {
  bedroom    = { climate = "climate.bedroom",       windows = { "binary_sensor.bedroom_window" } },
  livingroom = { climate = "climate.livingroom",    windows = { "binary_sensor.livingroom_window" } },
  childrens  = { climate = "climate.childrens_room", windows = { "binary_sensor.childrens_room_window" } },
}
function M.desired_key(zone)
  return "thermostat:desired:" .. zone
end
function M.written_key(zone)
  return "thermostat:written:" .. zone
end
return M
`

// writeTestZones drops the zones fixture into a test's lib dir.
func writeTestZones(t *testing.T, libDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(libDir, "zones.lua"), []byte(testZonesLua), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeThermostatScripts stages a scripts dir holding the shipped thermostat
// example and every lib it requires, plus the zones fixture, and returns it.
// One place to add a lib to: a script that gains a require and a test dir that
// does not simply fails to load.
func writeThermostatScripts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestZones(t, libDir)
	for _, lib := range []string{"schedule.lua", "control.lua", "climate.lua", "overshoot.lua"} {
		copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", lib), filepath.Join(libDir, lib))
	}
	copyRepoFile(t, filepath.Join(repoScriptsDir, "thermostat.lua"), filepath.Join(dir, "thermostat.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "thermostat.html"), filepath.Join(dir, "thermostat.html"))
	return dir
}

// TestWindowHandoffRestoresCommandedSetpoint exercises the two-script contract
// (spec §4.2): on a window close, the real heating_windows.lua must restore the
// setpoint the controller published to global:thermostat:written:<zone> — not a
// stale saved value, and not the *requested* value, which may sit above the
// commanded one while an overshoot correction is cutting a warmup short
// (overshoot-spec.md §7). The two keys are seeded to different values here so
// that distinction is pinned. It runs the shipped script in a real runner with
// a captured call_service and a seeded climate entity.
type windowSvcCall struct {
	domain, service string
	data            jsontext.Value
}

// windowHandoffHarness runs the shipped heating_windows.lua against a captured
// call_service, with climate.bedroom heating and both setpoints published to
// different values: 21 is what the user asked for, 20 is what is on the device.
type windowHandoffHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	mu      *sync.Mutex
	calls   *[]windowSvcCall
}

// windowZonesLua is a one-zone fixture whose window list the caller chooses;
// heating_windows.lua reads only these three fields.
const windowZonesLua = `local M = {}
M.frost_temp = 15
M.zones = { bedroom = { climate = "climate.bedroom", windows = { %s } } }
function M.desired_key(zone) return "thermostat:desired:" .. zone end
function M.written_key(zone) return "thermostat:written:" .. zone end
return M
`

// newWindowHandoffHarness binds `windows` to the bedroom zone (defaulting to
// one) and seeds the states in `seeded`. Everything is seeded in a single call
// because Seed replaces the mirror rather than adding to it.
func newWindowHandoffHarness(t *testing.T, windows []string, seeded map[string]string) *windowHandoffHarness {
	t.Helper()
	if len(windows) == 0 {
		windows = []string{"binary_sensor.bedroom_window"}
	}
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, 0, len(windows))
	for _, window := range windows {
		quoted = append(quoted, strconv.Quote(window))
	}
	zonesLua := fmt.Sprintf(windowZonesLua, strings.Join(quoted, ", "))
	if err := os.WriteFile(filepath.Join(libDir, "zones.lua"), []byte(zonesLua), 0o644); err != nil {
		t.Fatal(err)
	}
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "control.lua"), filepath.Join(libDir, "control.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "heating_windows.lua"), filepath.Join(dir, "heating_windows.lua"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	var mu sync.Mutex
	var calls []windowSvcCall
	cs := func(_ context.Context, domain, service string, data jsontext.Value) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, windowSvcCall{domain, service, data})
		return nil
	}

	sup := NewSupervisor(reg, dir, Deps{
		Tracker:     tracker,
		Scheduler:   sched,
		Global:      global,
		Root:        openTestRoot(t, dir),
		LogsRoot:    openTestRoot(t, t.TempDir()),
		NewKV:       func(id string) *store.Store { return store.New(writeDB, readDB, id) },
		CallService: cs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); sup.Wait() })

	seed := []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value("{}")},
	}
	for entityID, stateVal := range seeded {
		seed = append(seed, ha.StateData{
			EntityID: entityID, State: stateVal, Attributes: jsontext.Value("{}"),
		})
	}
	if err := tracker.Seed(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if err := global.Set(ctx, "thermostat:desired:bedroom", 21.0); err != nil {
		t.Fatal(err)
	}
	if err := global.Set(ctx, "thermostat:written:bedroom", 20.0); err != nil {
		t.Fatal(err)
	}
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	return &windowHandoffHarness{t: t, ctx: ctx, tracker: tracker, reg: reg, mu: &mu, calls: &calls}
}

func (h *windowHandoffHarness) report(entityID, oldState, newState string) {
	h.t.Helper()
	h.reg.Dispatch(ha.Event{
		Type: "state_changed",
		Data: jsontext.Value(`{"entity_id":"` + entityID + `",` +
			`"old_state":{"state":"` + oldState + `"},"new_state":{"state":"` + newState + `"}}`),
	})
}

// wroteSetpoint waits for a set_temperature(climate.bedroom, temp) call.
func (h *windowHandoffHarness) wroteSetpoint(temp float64) bool {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		snapshot := append([]windowSvcCall(nil), *h.calls...)
		h.mu.Unlock()
		for _, c := range snapshot {
			if c.domain != "climate" || c.service != "set_temperature" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(c.data, &m); err != nil {
				continue
			}
			if m["entity_id"] == "climate.bedroom" && m["temperature"] == temp {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestWindowHandoffRestoresCommandedSetpoint exercises the two-script contract
// (spec §4.2): on a window close, heating_windows.lua must restore the setpoint
// the controller published to global:thermostat:written:<zone> — not a stale
// saved value, and not the *requested* value, which may sit above the commanded
// one while an overshoot correction is cutting a warmup short (overshoot-spec.md
// §7).
func TestWindowHandoffRestoresCommandedSetpoint(t *testing.T) {
	h := newWindowHandoffHarness(t, nil, nil)

	h.report("binary_sensor.bedroom_window", "on", "off")
	if !h.wroteSetpoint(20) {
		t.Fatal("no set_temperature(climate.bedroom, 20) on the window close")
	}
}

// TestWindowHandoffWaitsForEveryWindow: the setpoint belongs to the zone, not to
// the sensor that fired, so closing one window while another in the same zone is
// still open must leave the frost guard in place.
func TestWindowHandoffWaitsForEveryWindow(t *testing.T) {
	h := newWindowHandoffHarness(t,
		[]string{"binary_sensor.bedroom_window", "binary_sensor.bedroom_window_2"},
		map[string]string{"binary_sensor.bedroom_window_2": "on"})

	h.report("binary_sensor.bedroom_window", "on", "off")
	if h.wroteSetpoint(20) {
		t.Fatal("restored the setpoint with the second window still open")
	}
}

// TestThermostatAPI loads the real thermostat.lua (with its libs, a real
// scheduler, and the Router wired up) and drives its HTTP API end to end:
// /api/state returns per-zone status, an override shows up in the next read,
// and a bad zone is rejected with 400.
func TestThermostatAPI(t *testing.T) {
	dir := writeThermostatScripts(t)

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, "thermostat")
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	router := NewRouter(reg)
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	// The override path calls set_temperature; a no-op capture keeps it from erroring.
	cs := func(context.Context, string, string, jsontext.Value) error { return nil }

	if err := tracker.Seed(context.Background(), []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":19.5,"temperature":18}`)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("thermostat", dir, openTestRoot(t, dir), openTestRoot(t, t.TempDir()), tracker, sched, kv, global)
	r.SetCallService(cs)
	reg.Add(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "thermostat.lua")) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("thermostat.lua did not finish loading")
	}
	router.Register("thermostat", r.Routes())

	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return m
	}

	// GET /api/state: bedroom present with its default override temp.
	rec := doReqID(router, "thermostat", "GET", "/api/state", "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/state status %d", rec.Code)
	}
	zones, _ := decode(rec)["zones"].(map[string]any)
	bedroom, _ := zones["bedroom"].(map[string]any)
	if bedroom == nil {
		t.Fatalf("no bedroom zone in state: %s", rec.Body.String())
	}
	if bedroom["override_temp"] != float64(21) {
		t.Errorf("override_temp = %v, want 21", bedroom["override_temp"])
	}
	if bedroom["mode"] != "heat" {
		t.Errorf("mode = %v, want heat", bedroom["mode"])
	}
	// No schedule and no override: there is no request, so `target` falls back
	// to the device's own setpoint and the two halves of the split agree.
	if bedroom["target"] != float64(18) || bedroom["commanded"] != float64(18) {
		t.Errorf("target/commanded = %v/%v, want 18/18", bedroom["target"], bedroom["commanded"])
	}

	// POST /api/override: the override is reflected in the returned state.
	rec = doReqID(router, "thermostat", "POST", "/api/override", `{"zone":"bedroom","minutes":30}`)
	if rec.Code != 200 {
		t.Fatalf("POST /api/override status %d body %q", rec.Code, rec.Body.String())
	}
	zones, _ = decode(rec)["zones"].(map[string]any)
	bedroom, _ = zones["bedroom"].(map[string]any)
	override, _ := bedroom["override"].(map[string]any)
	if override == nil || override["active"] != true {
		t.Fatalf("override not active after POST: %s", rec.Body.String())
	}
	if rem, _ := override["remaining_s"].(float64); rem <= 0 || rem > 30*60 {
		t.Errorf("remaining_s = %v, want 0<rem<=1800", override["remaining_s"])
	}
	// The split (overshoot-spec.md §8): the override makes the request 21 while
	// call_service is a no-op capture, so the entity's setpoint stays 18. This
	// is the only place the two halves are forced apart — `target` must be what
	// was asked for, `commanded` what is on the device.
	if bedroom["target"] != float64(21) {
		t.Errorf("target = %v, want 21 (the requested override temp)", bedroom["target"])
	}
	if bedroom["commanded"] != float64(18) {
		t.Errorf("commanded = %v, want 18 (the entity's setpoint)", bedroom["commanded"])
	}

	// Bad zone -> 400.
	rec = doReqID(router, "thermostat", "POST", "/api/override", `{"zone":"nope","minutes":30}`)
	if rec.Code != 400 {
		t.Fatalf("bad zone status = %d, want 400", rec.Code)
	}

	// Override temp is bounded by the device's advertised max_temp. Re-seed the
	// bedroom with a 30° ceiling: a PUT above it must be rejected (this is the
	// bug where HA silently drops a setpoint above max_temp and the device never
	// heats to it), while a value inside the range is accepted and echoed back
	// along with the bounds.
	if err := tracker.Seed(context.Background(), []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":19.5,"temperature":18,"min_temp":7,"max_temp":30}`)},
	}); err != nil {
		t.Fatal(err)
	}
	rec = doReqID(router, "thermostat", "PUT", "/api/settings", `{"zone":"bedroom","override_temp":31.3}`)
	if rec.Code != 400 {
		t.Errorf("override_temp above max_temp: status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	rec = doReqID(router, "thermostat", "PUT", "/api/settings", `{"zone":"bedroom","override_temp":29}`)
	if rec.Code != 200 {
		t.Fatalf("override_temp within range: status = %d body %q", rec.Code, rec.Body.String())
	}
	zones, _ = decode(rec)["zones"].(map[string]any)
	bedroom, _ = zones["bedroom"].(map[string]any)
	if bedroom["override_temp"] != float64(29) {
		t.Errorf("override_temp = %v, want 29", bedroom["override_temp"])
	}
	if bedroom["max_temp"] != float64(30) {
		t.Errorf("max_temp = %v, want 30", bedroom["max_temp"])
	}

	// The schedule editor is bounded by the same device range: a transition
	// above max_temp is rejected, one inside it is accepted.
	rec = doReqID(router, "thermostat", "PUT", "/api/schedule", `{"zone":"bedroom","days":{"0":[{"time":"06:00","temp":33}]}}`)
	if rec.Code != 400 {
		t.Errorf("schedule temp above max_temp: status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	rec = doReqID(router, "thermostat", "PUT", "/api/schedule", `{"zone":"bedroom","days":{"0":[{"time":"06:00","temp":22}]}}`)
	if rec.Code != 200 {
		t.Fatalf("schedule temp within range: status = %d body %q", rec.Code, rec.Body.String())
	}

	// The learner's own endpoints (§9.5, §9.6): its state is curl-able, and a
	// zone that has gone wrong is recoverable without touching the database.
	rec = doReqID(router, "thermostat", "GET", "/api/overshoot?zone=bedroom", "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/overshoot status %d body %q", rec.Code, rec.Body.String())
	}
	learner := decode(rec)
	if learner["c"] != float64(0) {
		t.Errorf("c = %v, want 0 (nothing learned)", learner["c"])
	}
	if learner["observe_only"] != true {
		t.Errorf("observe_only = %v, want true — it ships watching", learner["observe_only"])
	}
	if rec := doReqID(router, "thermostat", "GET", "/api/overshoot?zone=nope", ""); rec.Code != 400 {
		t.Errorf("unknown zone: status = %d, want 400", rec.Code)
	}

	rec = doReqID(router, "thermostat", "POST", "/api/overshoot/observe", `{"zone":"bedroom","observe_only":false}`)
	if rec.Code != 200 {
		t.Fatalf("POST /api/overshoot/observe status %d body %q", rec.Code, rec.Body.String())
	}
	zones, _ = decode(rec)["zones"].(map[string]any)
	bedroom, _ = zones["bedroom"].(map[string]any)
	if bedroom["observe_only"] != false {
		t.Errorf("observe_only = %v after opting in, want false", bedroom["observe_only"])
	}
	if rec := doReqID(router, "thermostat", "POST", "/api/overshoot/observe", `{"zone":"bedroom","observe_only":"no"}`); rec.Code != 400 {
		t.Errorf("non-boolean observe_only: status = %d, want 400", rec.Code)
	}

	// Reset restores the untrained state, flag included.
	if err := kv.Set(context.Background(), "overshoot_c:bedroom", 0.05); err != nil {
		t.Fatal(err)
	}
	rec = doReqID(router, "thermostat", "POST", "/api/overshoot/reset", `{"zone":"bedroom"}`)
	if rec.Code != 200 {
		t.Fatalf("POST /api/overshoot/reset status %d body %q", rec.Code, rec.Body.String())
	}
	zones, _ = decode(rec)["zones"].(map[string]any)
	bedroom, _ = zones["bedroom"].(map[string]any)
	if bedroom["c"] != float64(0) || bedroom["samples"] != float64(0) {
		t.Errorf("after reset c/samples = %v/%v, want 0/0", bedroom["c"], bedroom["samples"])
	}

	// GET / serves the self-contained UI page.
	rec = doReqID(router, "thermostat", "GET", "/", "")
	if rec.Code != 200 {
		t.Fatalf("GET / status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("GET / did not return the HTML page")
	}
}

// TestThermostatOrderAPI exercises the card-order persistence: GET /api/state
// reports the order (alphabetical until set), PUT /api/order persists an
// arbitrary arrangement that survives a re-GET (so other browsers see it),
// unknown zones are rejected, and a partial list appends the omitted zones.
func TestThermostatOrderAPI(t *testing.T) {
	srv := serveThermostatUISeed(t, defaultZoneSeed())
	client := srv.Client()

	getOrder := func() []string {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/s/thermostat/api/state", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Order []string `json:"order"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Order
	}
	putOrder := func(payload string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), "PUT", srv.URL+"/s/thermostat/api/order", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Default order is alphabetical by zone key.
	if got := getOrder(); !reflect.DeepEqual(got, []string{"bedroom", "childrens", "livingroom"}) {
		t.Fatalf("default order = %v, want alphabetical", got)
	}

	// A full reordering is echoed back and persists for the next reader.
	resp := putOrder(`{"order":["livingroom","bedroom","childrens"]}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT /api/order status = %d", resp.StatusCode)
	}
	want := []string{"livingroom", "bedroom", "childrens"}
	if got := getOrder(); !reflect.DeepEqual(got, want) {
		t.Errorf("persisted order = %v, want %v", got, want)
	}

	// An unknown zone is rejected and leaves the stored order untouched.
	bad := putOrder(`{"order":["nope"]}`)
	defer bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Errorf("unknown-zone order status = %d, want 400", bad.StatusCode)
	}
	if got := getOrder(); !reflect.DeepEqual(got, want) {
		t.Errorf("order after rejected PUT = %v, want unchanged %v", got, want)
	}

	// A partial list keeps the named zone first and appends the rest alphabetically.
	partial := putOrder(`{"order":["livingroom"]}`)
	defer partial.Body.Close()
	if partial.StatusCode != 200 {
		t.Fatalf("partial order status = %d", partial.StatusCode)
	}
	if got := getOrder(); !reflect.DeepEqual(got, []string{"livingroom", "bedroom", "childrens"}) {
		t.Errorf("partial order = %v, want livingroom then alphabetical rest", got)
	}
}

// startThermostat loads the real thermostat.lua (with libs + a real scheduler)
// and returns the pieces needed to seed state and dispatch events at it. The
// scheduler is created but not Start()ed, so no tick fires and tests drive the
// controller purely through dispatched state-change events.
// startThermostat loads the real thermostat.lua in a runner. Any prepare
// functions run against the script's KV store BEFORE the script loads, which is
// how a test stages state the script only reads at load time.
func startThermostat(t *testing.T, prepare ...func(context.Context, *store.Store)) (*Registry, *store.Store, *store.GlobalStore, *state.Tracker) {
	t.Helper()
	dir := writeThermostatScripts(t)

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, "thermostat")
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	for _, fn := range prepare {
		fn(context.Background(), kv)
	}

	r := NewRunner("thermostat", dir, openTestRoot(t, dir), openTestRoot(t, t.TempDir()), tracker, sched, kv, global)
	r.SetCallService(func(context.Context, string, string, jsontext.Value) error { return nil })
	reg.Add(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "thermostat.lua")) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("thermostat.lua did not finish loading")
	}
	return reg, kv, global, tracker
}

// climateChange builds a state_changed event for a heating climate entity whose
// target setpoint moved from oldT to newT.
func climateChange(entity string, oldT, newT float64) ha.Event {
	return ha.Event{Type: "state_changed", Data: jsontext.Value(fmt.Sprintf(
		`{"entity_id":%q,"old_state":{"state":"heat","attributes":{"temperature":%v}},`+
			`"new_state":{"state":"heat","attributes":{"temperature":%v}}}`, entity, oldT, newT))}
}

// climateChangeInRoom is climateChange plus the room temperature, which the
// overshoot learner needs: the tracker replaces an entity's attributes
// wholesale, so an event that omits current_temperature erases it.
func climateChangeInRoom(entity string, oldT, newT, room float64) ha.Event {
	return ha.Event{Type: "state_changed", Data: jsontext.Value(fmt.Sprintf(
		`{"entity_id":%q,"old_state":{"state":"heat","attributes":{"temperature":%v,"current_temperature":%v}},`+
			`"new_state":{"state":"heat","attributes":{"temperature":%v,"current_temperature":%v}}}`,
		entity, oldT, room, newT, room))}
}

// relayClosed is the device reporting its relay switching on inside a hold:
// the setpoint and the room are unchanged, only hvac_action moves. Unlike the
// dispatch-only helpers above it carries entity_id inside new_state, because
// the tracker keys the mirror on that and this event is applied to it too.
func relayClosed(entity string, target, room float64) ha.Event {
	return ha.Event{Type: "state_changed", Data: jsontext.Value(fmt.Sprintf(
		`{"entity_id":%[1]q,"old_state":{"entity_id":%[1]q,"state":"heat","attributes":{"temperature":%[2]v,"current_temperature":%[3]v,"hvac_action":"idle"}},`+
			`"new_state":{"entity_id":%[1]q,"state":"heat","attributes":{"temperature":%[2]v,"current_temperature":%[3]v,"hvac_action":"heating"}}}`,
		entity, target, room))}
}

func manualTemp(t *testing.T, kv *store.Store, zone string) (float64, bool) {
	t.Helper()
	v, err := kv.Get(context.Background(), "manual:"+zone)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	temp, _ := m["temp"].(float64)
	return temp, true
}

// TestThermostatManualHoldDetected: with no override and a closed (seeded)
// window, a climate target that differs from the published *written* setpoint
// is recorded as a manual hold (§9), with a future expiry. The two published
// keys are seeded apart and the dial is moved to exactly the requested value,
// so a detector comparing against `desired` instead would see no change at all.
func TestThermostatManualHoldDetected(t *testing.T) {
	reg, kv, global, tracker := startThermostat(t)
	ctx := context.Background()

	if err := tracker.Seed(ctx, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"temperature":18}`)},
		{EntityID: "binary_sensor.bedroom_window", State: "off", Attributes: jsontext.Value("{}")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := global.Set(ctx, "thermostat:desired:bedroom", 20.0); err != nil {
		t.Fatal(err)
	}
	if err := global.Set(ctx, "thermostat:written:bedroom", 18.0); err != nil {
		t.Fatal(err)
	}

	reg.Dispatch(climateChange("climate.bedroom", 18, 20))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if temp, ok := manualTemp(t, kv, "bedroom"); ok {
			if temp != 20 {
				t.Fatalf("manual temp = %v, want 20", temp)
			}
			ov, _ := kv.Get(ctx, "manual:bedroom")
			m := ov.(map[string]any)
			exp, _ := m["expires"].(string)
			if _, err := time.Parse(time.RFC3339, exp); err != nil {
				t.Fatalf("manual expires not RFC3339: %q", exp)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("manual hold was never recorded")
}

// TestThermostatOpensOvershootEpisode: a request that rises above the room
// temperature starts a run's episode (overshoot-spec.md §5), and opening it
// decides nothing: no hold before the radiator is seen warming.
func TestThermostatOpensOvershootEpisode(t *testing.T) {
	reg, kv, global, tracker := startThermostat(t)
	ctx := context.Background()

	if err := tracker.Seed(ctx, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"temperature":18,"current_temperature":18}`)},
		{EntityID: "binary_sensor.bedroom_window", State: "off", Attributes: jsontext.Value("{}")},
	}); err != nil {
		t.Fatal(err)
	}
	_ = global.Set(ctx, "thermostat:desired:bedroom", 18.0)
	_ = global.Set(ctx, "thermostat:written:bedroom", 18.0)

	// The dial moving to 21 becomes a manual hold, which re-applies the zone —
	// the request changes from 18 to 21 with the room at 18, so an episode opens.
	reg.Dispatch(climateChangeInRoom("climate.bedroom", 18, 21, 18))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := kv.Get(ctx, "overshoot_episode:bedroom")
		if err != nil {
			t.Fatal(err)
		}
		episode, ok := v.(map[string]any)
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if episode["requested"] != float64(21) {
			t.Errorf("requested = %v, want 21", episode["requested"])
		}
		if episode["rise"] != float64(3) {
			t.Errorf("rise = %v, want 3", episode["rise"])
		}
		if episode["c_used"] != float64(0) {
			t.Errorf("c_used = %v, want 0 (nothing learned yet)", episode["c_used"])
		}
		if episode["observe_only"] != true {
			t.Errorf("observe_only = %v, want true (it ships watching, §9.4)", episode["observe_only"])
		}
		if episode["hold"] != nil {
			t.Errorf("hold = %v at the open: a run is never cut before the radiator warms", episode["hold"])
		}
		return
	}
	t.Fatal("no overshoot episode was opened")
}

// TestThermostatOpensEpisodeOnHeating: the second trigger of spec §5. A hold
// whose request never moves still opens an episode when the relay closes —
// the hold is seeded, desired and written agree with the dial so nothing reads
// as manual, and the only thing that changes is hvac_action.
func TestThermostatOpensEpisodeOnHeating(t *testing.T) {
	reg, kv, global, tracker := startThermostat(t, func(ctx context.Context, kv *store.Store) {
		if err := kv.Set(ctx, "manual:bedroom", map[string]any{
			"temp": 21.0, "expires": time.Now().Add(6 * time.Hour).Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()

	if err := tracker.Seed(ctx, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"temperature":21,"current_temperature":20.6,"hvac_action":"idle"}`)},
		{EntityID: "binary_sensor.bedroom_window", State: "off", Attributes: jsontext.Value("{}")},
	}); err != nil {
		t.Fatal(err)
	}
	_ = global.Set(ctx, "thermostat:desired:bedroom", 21.0)
	_ = global.Set(ctx, "thermostat:written:bedroom", 21.0)

	// Applied to the mirror BEFORE the dispatch, as main.go orders it: the
	// trigger reads hvac_action off the mirror, not off the event.
	ev := relayClosed("climate.bedroom", 21, 20.6)
	if err := tracker.HandleStateChanged(ctx, ev.Data); err != nil {
		t.Fatal(err)
	}
	reg.Dispatch(ev)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := kv.Get(ctx, "overshoot_episode:bedroom")
		if err != nil {
			t.Fatal(err)
		}
		episode, ok := v.(map[string]any)
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if episode["requested"] != float64(21) {
			t.Errorf("requested = %v, want 21", episode["requested"])
		}
		if rise, _ := episode["rise"].(float64); rise < 0.39 || rise > 0.41 {
			t.Errorf("rise = %v, want 0.4 (the deadband)", episode["rise"])
		}
		if episode["heated"] != true {
			t.Errorf("heated = %v, want true from the open", episode["heated"])
		}
		return
	}
	t.Fatal("the relay closing opened no episode")
}

// TestThermostatAbandonsEpisodeOnRestart: an episode still in flight when the
// daemon stopped is discarded at load, not resumed — its timing is broken and a
// corrupted k costs more than a lost sample (§6). It must leave a journal row
// with the reason, because an episode that vanishes silently is exactly the
// failure §9.1 is about.
func TestThermostatAbandonsEpisodeOnRestart(t *testing.T) {
	stale := map[string]any{
		"opened_at": 1000, "requested": 21.0, "current_at_open": 18.0,
		"rise": 3.0, "c_used": 0.02,
		"applied": 19.8, "observe_only": false, "peak": 19.9, "peak_at": 1200,
	}
	_, kv, _, _ := startThermostat(t, func(ctx context.Context, kv *store.Store) {
		if err := kv.Set(ctx, "overshoot_episode:bedroom", stale); err != nil {
			t.Fatal(err)
		}
		if err := kv.Set(ctx, "overshoot_c:bedroom", 0.02); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()

	if v, err := kv.Get(ctx, "overshoot_episode:bedroom"); err != nil {
		t.Fatal(err)
	} else if v != nil {
		t.Errorf("stale episode survived the load: %v", v)
	}

	v, err := kv.Get(ctx, "overshoot_journal:bedroom")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := v.([]any)
	if len(rows) != 1 {
		t.Fatalf("journal has %d rows, want 1: %v", len(rows), v)
	}
	row, _ := rows[0].(map[string]any)
	if row["outcome"] != "discarded" || row["reason"] != "restart" {
		t.Errorf("outcome/reason = %v/%v, want discarded/restart", row["outcome"], row["reason"])
	}
	if row["c_before"] != 0.02 || row["c_after"] != 0.02 {
		t.Errorf("c moved on a discarded episode: %v -> %v", row["c_before"], row["c_after"])
	}

	// A discard must not count as a sample; nothing was learned.
	if v, err := kv.Get(ctx, "overshoot_c_samples:bedroom"); err != nil {
		t.Fatal(err)
	} else if v != nil {
		t.Errorf("samples = %v, want unset after a discard", v)
	}
}

// TestThermostatOverrideSuppressesManual: an active override makes the
// controller ignore manual dial changes (§5.3a). A second zone with no override
// acts as a FIFO barrier — once its manual hold appears, the overridden zone's
// event has already been processed, so the absence of its hold is deterministic.
func TestThermostatOverrideSuppressesManual(t *testing.T) {
	reg, kv, global, tracker := startThermostat(t)
	ctx := context.Background()

	if err := kv.Set(ctx, "override:bedroom", map[string]any{
		"active":  true,
		"ends_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Seed(ctx, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"temperature":18}`)},
		{EntityID: "binary_sensor.bedroom_window", State: "off", Attributes: jsontext.Value("{}")},
		{EntityID: "climate.childrens_room", State: "heat", Attributes: jsontext.Value(`{"temperature":18}`)},
		{EntityID: "binary_sensor.childrens_room_window", State: "off", Attributes: jsontext.Value("{}")},
	}); err != nil {
		t.Fatal(err)
	}
	_ = global.Set(ctx, "thermostat:desired:bedroom", 18.0)
	_ = global.Set(ctx, "thermostat:desired:childrens", 18.0)
	_ = global.Set(ctx, "thermostat:written:bedroom", 18.0)
	_ = global.Set(ctx, "thermostat:written:childrens", 18.0)

	reg.Dispatch(climateChange("climate.bedroom", 18, 22))        // must be suppressed (override)
	reg.Dispatch(climateChange("climate.childrens_room", 18, 22)) // barrier: must create manual hold

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := manualTemp(t, kv, "childrens"); ok {
			if _, ok := manualTemp(t, kv, "bedroom"); ok {
				t.Fatal("active override did not suppress the manual hold")
			}
			return // barrier processed and bedroom has no manual hold
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("barrier manual hold (childrens) never appeared")
}
