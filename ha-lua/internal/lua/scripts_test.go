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

		-- is_manual: within 0.1 of a numeric published value is our own write.
		assert(c.is_manual(21.0, 21) == false, "21 vs 21.0 not manual")
		assert(c.is_manual(21.05, 21) == false, "within tolerance not manual")
		assert(c.is_manual(21.2, 21) == true, "beyond tolerance is manual")
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

		-- offset: proportional to the rise, clamped into [0, MAX_OFFSET].
		assert(o.offset(0, 3) == 0, "k=0 -> no correction")
		assert(math.abs(o.offset(0.4, 3) - 1.2) < 1e-9, "0.4 * 3 = 1.2")
		assert(o.offset(0.8, 10) == o.MAX_OFFSET, "clamped to MAX_OFFSET")
		assert(o.offset(0.4, -2) == 0, "negative rise -> no correction")
		-- A top-up gets essentially nothing, which is what keeps the correction
		-- out of the steady-state hold band.
		assert(o.offset(0.4, 0.3) < 0.13, "top-up correction is negligible")

		-- open: only when the request is above the room.
		assert(o.open(21, 21, 0.4, false, 0) == nil, "no rise -> no episode")
		assert(o.open(21, 22, 0.4, false, 0) == nil, "falling request -> no episode")
		local ep = o.open(21, 18, 0.4, false, 1000)
		assert(ep.rise == 3, "rise")
		assert(math.abs(ep.offset - 1.2) < 1e-9, "offset latched")
		assert(math.abs(ep.commanded - 19.8) < 1e-9, "commanded")
		assert(ep.applied == ep.commanded, "correcting -> applied is commanded")
		assert(ep.peak == 18 and ep.peak_at == 1000, "peak seeded at the room temp")

		-- Observe-only writes the request but records what it would have done.
		local obs = o.open(21, 18, 0.4, true, 1000)
		assert(obs.applied == 21, "observe-only applies the request")
		assert(math.abs(obs.commanded - 19.8) < 1e-9, "observe-only still records the command")

		-- step: climb, cut off at the applied setpoint, then coast.
		local phase = o.step(ep, 19, 1060)
		assert(phase == "heating" and ep.peak == 19, "still climbing")
		assert(ep.cutoff_at == nil, "no cutoff yet")
		phase = o.step(ep, 19.9, 1120)
		assert(phase == "coasting" and ep.cutoff_at == 1120, "cut off at the commanded value")
		phase = o.step(ep, 20.6, 1300)
		assert(phase == "coasting" and ep.peak == 20.6 and ep.peak_at == 1300, "peak tracked while coasting")
		phase = o.step(ep, 20.2, 1400)
		assert(phase == "coasting" and ep.peak == 20.6, "peak is a running max, not the last sample")
		phase = o.step(ep, 20.1, 1120 + o.COAST_SECONDS)
		assert(phase == "done", "coast window closes the episode")

		-- close: the peak landed 0.4 below the request, so k comes down.
		local k_after, outcome, reason = o.close(ep, 0.4)
		assert(outcome == "learned" and reason == nil, "learned")
		-- error = 20.6 - 21 = -0.4, rise 3 -> k + 0.5 * (-0.4/3)
		assert(math.abs(k_after - (0.4 + 0.5 * (-0.4 / 3))) < 1e-9, "k update "..tostring(k_after))
		assert(o.close(o.open(21, 18, 0.4, true, 0), 0.4) ~= nil, "observe-only still learns")

		-- k stays inside its bounds however extreme the error.
		local hot = o.open(21, 18, 0.4, false, 0)
		o.step(hot, 19.8, 60)
		o.step(hot, 40, 120)
		o.step(hot, 40, 60 + o.COAST_SECONDS + 120)
		local k_hot = o.close(hot, 0.4)
		assert(k_hot == o.K_MAX, "k clamped to K_MAX, got "..tostring(k_hot))
		-- Downward, k can only ever halve toward zero and never cross it: an
		-- episode cuts off at requested - k*rise, so the peak cannot land more
		-- than the offset low and the update is bounded below by -GAIN*k. The
		-- zero clamp is defensive, not reachable.
		local cold = o.open(21, 18, 0.4, false, 0)
		o.step(cold, 19.8, 60)
		o.step(cold, 19.8, 60 + o.COAST_SECONDS)
		local k_cold = o.close(cold, 0.4)
		assert(math.abs(k_cold - 0.2) < 1e-9, "worst case halves k, got "..tostring(k_cold))

		-- valid returns the reason, not a bare boolean (spec §9.2), and the
		-- FIRST reason wins so the thing that actually broke the episode is what
		-- a reader sees.
		local windowed = o.open(21, 18, 0.4, false, 0)
		o.invalidate(windowed, "window_open")
		o.invalidate(windowed, "mode_left_heat")
		local ok, why = o.valid(windowed)
		assert(ok == false and why == "window_open", "first reason wins, got "..tostring(why))
		local k_same, outcome2, reason2 = o.close(windowed, 0.4)
		assert(k_same == 0.4 and outcome2 == "discarded" and reason2 == "window_open", "discard leaves k alone")

		-- A rise too small to teach anything still gets its (tiny) correction.
		local tiny = o.open(21, 20.9, 0.4, false, 0)
		assert(tiny ~= nil and tiny.offset > 0, "tiny episode still corrects")
		o.step(tiny, 21, 60)
		o.step(tiny, 21.4, 60 + o.COAST_SECONDS)
		ok, why = o.valid(tiny)
		assert(ok == false and why == "rise_too_small", "tiny rise teaches nothing, got "..tostring(why))

		-- An episode that never reaches its setpoint is abandoned rather than
		-- left open forever learning nothing.
		local stuck = o.open(21, 18, 0, false, 0)
		assert(o.step(stuck, 18.5, 3600) == "heating", "still trying")
		assert(o.step(stuck, 18.6, o.MAX_EPISODE_SECONDS) == "done", "given up")
		ok, why = o.valid(stuck)
		assert(ok == false and why == "never_reached", "never_reached, got "..tostring(why))

		-- record carries both the decision and the outcome.
		local row = o.record(ep, "childrens", 0.4, k_after, "learned", nil, 9999)
		assert(row.zone == "childrens" and row.closed_at == 9999, "record identity")
		assert(row.k_before == 0.4 and row.k_after == k_after, "record k")
		assert(math.abs(row.error - (20.6 - 21)) < 1e-9, "record error")
		assert(row.outcome == "learned" and row.reason == nil, "record outcome")
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
	for _, lib := range []string{"schedule.lua", "control.lua", "overshoot.lua"} {
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
func TestWindowHandoffRestoresCommandedSetpoint(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestZones(t, libDir)
	copyRepoFile(t, filepath.Join(repoScriptsDir, "heating_windows.lua"), filepath.Join(dir, "heating_windows.lua"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	type svcCall struct {
		domain, service string
		data            jsontext.Value
	}
	var mu sync.Mutex
	var calls []svcCall
	cs := func(_ context.Context, domain, service string, data jsontext.Value) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, svcCall{domain, service, data})
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
	defer func() { cancel(); sup.Wait() }()

	// The zone must be heating, and the controller has published both setpoints.
	// They differ: 21 is what the user asked for, 20 is what is on the device.
	if err := tracker.Seed(ctx, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value("{}")},
	}); err != nil {
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

	// The window closes: heating_windows must restore the published desired.
	reg.Dispatch(ha.Event{
		Type: "state_changed",
		Data: jsontext.Value(`{"entity_id":"binary_sensor.bedroom_window",` +
			`"old_state":{"state":"on"},"new_state":{"state":"off"}}`),
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		snapshot := append([]svcCall(nil), calls...)
		mu.Unlock()
		for _, c := range snapshot {
			if c.domain != "climate" || c.service != "set_temperature" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(c.data, &m); err != nil {
				continue
			}
			if m["entity_id"] == "climate.bedroom" && m["temperature"] == float64(20) {
				return // handoff worked
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("no set_temperature(climate.bedroom, 20) call; got %+v", calls)
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
	if learner["k"] != float64(0) {
		t.Errorf("k = %v, want 0 (K_INIT)", learner["k"])
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
	if err := kv.Set(context.Background(), "overshoot_k:bedroom", 0.5); err != nil {
		t.Fatal(err)
	}
	rec = doReqID(router, "thermostat", "POST", "/api/overshoot/reset", `{"zone":"bedroom"}`)
	if rec.Code != 200 {
		t.Fatalf("POST /api/overshoot/reset status %d body %q", rec.Code, rec.Body.String())
	}
	zones, _ = decode(rec)["zones"].(map[string]any)
	bedroom, _ = zones["bedroom"].(map[string]any)
	if bedroom["k"] != float64(0) || bedroom["samples"] != float64(0) {
		t.Errorf("after reset k/samples = %v/%v, want 0/0", bedroom["k"], bedroom["samples"])
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
// temperature starts an overshoot episode (overshoot-spec.md §5). k is still
// K_INIT here, so the latched offset is zero and the commanded setpoint equals
// the request — the learner records the episode without changing behaviour.
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
		if episode["offset"] != float64(0) {
			t.Errorf("offset = %v, want 0 (K_INIT is zero)", episode["offset"])
		}
		if episode["observe_only"] != true {
			t.Errorf("observe_only = %v, want true (it ships watching, §9.4)", episode["observe_only"])
		}
		if episode["applied"] != float64(21) {
			t.Errorf("applied = %v, want 21 (the uncorrected request)", episode["applied"])
		}
		return
	}
	t.Fatal("no overshoot episode was opened")
}

// TestThermostatAbandonsEpisodeOnRestart: an episode still in flight when the
// daemon stopped is discarded at load, not resumed — its timing is broken and a
// corrupted k costs more than a lost sample (§6). It must leave a journal row
// with the reason, because an episode that vanishes silently is exactly the
// failure §9.1 is about.
func TestThermostatAbandonsEpisodeOnRestart(t *testing.T) {
	stale := map[string]any{
		"opened_at": 1000, "requested": 21.0, "current_at_open": 18.0,
		"rise": 3.0, "k_used": 0.4, "offset": 1.2, "commanded": 19.8,
		"applied": 19.8, "observe_only": false, "peak": 19.9, "peak_at": 1200,
	}
	_, kv, _, _ := startThermostat(t, func(ctx context.Context, kv *store.Store) {
		if err := kv.Set(ctx, "overshoot_episode:bedroom", stale); err != nil {
			t.Fatal(err)
		}
		if err := kv.Set(ctx, "overshoot_k:bedroom", 0.4); err != nil {
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
	if row["k_before"] != float64(0.4) || row["k_after"] != float64(0.4) {
		t.Errorf("k moved on a discarded episode: %v -> %v", row["k_before"], row["k_after"])
	}

	// A discard must not count as a sample; nothing was learned.
	if v, err := kv.Get(ctx, "overshoot_samples:bedroom"); err != nil {
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
