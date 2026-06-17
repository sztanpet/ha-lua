package lua

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-json-experiment/json/jsontext"
	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// repoScriptsDir is the shipped example/script tree, relative to this package.
const repoScriptsDir = "../../scripts"

// TestShippedScriptsCompile loads every *.lua under scripts/ (compile only, no
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
// scripts/lib, so tests exercise the actual shipped lib/schedule.lua.
func newScheduleState(t *testing.T) *lua.LState {
	t.Helper()
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	L.SetContext(context.Background())
	t.Cleanup(L.Close)
	RegisterStdlib(L, repoScriptsDir)
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

// TestWindowHandoffRestoresPublishedDesired exercises the two-script contract
// (spec §4.2): on a window close, the real heating_windows.lua must restore the
// setpoint the controller published to global:thermostat:desired:<zone> — not a
// stale saved value. It runs the shipped script in a real runner with a captured
// call_service, a seeded climate entity, and a published desired.
func TestWindowHandoffRestoresPublishedDesired(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "zones.lua"), filepath.Join(libDir, "zones.lua"))
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
		NewKV:       func(id string) *store.Store { return store.New(writeDB, readDB, id) },
		CallService: cs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); sup.Wait() }()

	// The zone must be heating, and the controller has published its desired.
	if err := tracker.Seed(ctx, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value("{}")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := global.Set(ctx, "thermostat:desired:bedroom", 20.0); err != nil {
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
// /api/state returns per-zone status, a boost shows up in the next read, and a
// bad zone is rejected with 400.
func TestThermostatAPI(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "zones.lua"), filepath.Join(libDir, "zones.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "schedule.lua"), filepath.Join(libDir, "schedule.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "thermostat.lua"), filepath.Join(dir, "thermostat.lua"))

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

	// The boost path calls set_temperature; a no-op capture keeps it from erroring.
	cs := func(context.Context, string, string, jsontext.Value) error { return nil }

	if err := tracker.Seed(context.Background(), []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"current_temperature":19.5,"temperature":18}`)},
	}); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("thermostat", dir, tracker, sched, kv, global)
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

	// GET /api/state: bedroom present with its default comfort temp.
	rec := doReq(router, "GET", "/api/state", "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/state status %d", rec.Code)
	}
	zones, _ := decode(rec)["zones"].(map[string]any)
	bedroom, _ := zones["bedroom"].(map[string]any)
	if bedroom == nil {
		t.Fatalf("no bedroom zone in state: %s", rec.Body.String())
	}
	if bedroom["comfort_temp"] != float64(21) {
		t.Errorf("comfort_temp = %v, want 21", bedroom["comfort_temp"])
	}
	if bedroom["mode"] != "heat" {
		t.Errorf("mode = %v, want heat", bedroom["mode"])
	}

	// POST /api/boost: the boost is reflected in the returned state.
	rec = doReq(router, "POST", "/api/boost", `{"zone":"bedroom","minutes":30}`)
	if rec.Code != 200 {
		t.Fatalf("POST /api/boost status %d body %q", rec.Code, rec.Body.String())
	}
	zones, _ = decode(rec)["zones"].(map[string]any)
	bedroom, _ = zones["bedroom"].(map[string]any)
	boost, _ := bedroom["boost"].(map[string]any)
	if boost == nil || boost["active"] != true {
		t.Fatalf("boost not active after POST: %s", rec.Body.String())
	}
	if rem, _ := boost["remaining_s"].(float64); rem <= 0 || rem > 30*60 {
		t.Errorf("remaining_s = %v, want 0<rem<=1800", boost["remaining_s"])
	}

	// Bad zone -> 400.
	rec = doReq(router, "POST", "/api/boost", `{"zone":"nope","minutes":30}`)
	if rec.Code != 400 {
		t.Fatalf("bad zone status = %d, want 400", rec.Code)
	}

	// GET / serves the self-contained UI page.
	rec = doReq(router, "GET", "/", "")
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

// startThermostat loads the real thermostat.lua (with libs + a real scheduler)
// and returns the pieces needed to seed state and dispatch events at it. The
// scheduler is created but not Start()ed, so no tick fires and tests drive the
// controller purely through dispatched state-change events.
func startThermostat(t *testing.T) (*Registry, *store.Store, *store.GlobalStore, *state.Tracker) {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "zones.lua"), filepath.Join(libDir, "zones.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "schedule.lua"), filepath.Join(libDir, "schedule.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "thermostat.lua"), filepath.Join(dir, "thermostat.lua"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, "thermostat")
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	r := NewRunner("thermostat", dir, tracker, sched, kv, global)
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

func overrideTemp(t *testing.T, kv *store.Store, zone string) (float64, bool) {
	t.Helper()
	v, err := kv.Get(context.Background(), "override:"+zone)
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

// TestThermostatManualOverrideDetected: with no boost and a closed (seeded)
// window, a climate target that differs from the published desired is recorded
// as a manual override (§9), with a future expiry.
func TestThermostatManualOverrideDetected(t *testing.T) {
	reg, kv, global, tracker := startThermostat(t)
	ctx := context.Background()

	if err := tracker.Seed(ctx, []ha.StateData{
		{EntityID: "climate.bedroom", State: "heat", Attributes: jsontext.Value(`{"temperature":18}`)},
		{EntityID: "binary_sensor.bedroom_window", State: "off", Attributes: jsontext.Value("{}")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := global.Set(ctx, "thermostat:desired:bedroom", 18.0); err != nil {
		t.Fatal(err)
	}

	reg.Dispatch(climateChange("climate.bedroom", 18, 22))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if temp, ok := overrideTemp(t, kv, "bedroom"); ok {
			if temp != 22 {
				t.Fatalf("override temp = %v, want 22", temp)
			}
			ov, _ := kv.Get(ctx, "override:bedroom")
			m := ov.(map[string]any)
			exp, _ := m["expires"].(string)
			if _, err := time.Parse(time.RFC3339, exp); err != nil {
				t.Fatalf("override expires not RFC3339: %q", exp)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("manual override was never recorded")
}

// TestThermostatBoostSuppressesOverride: an active boost makes the controller
// ignore manual dial changes (§5.3a). A second zone with no boost acts as a
// FIFO barrier — once its override appears, the boosted zone's event has
// already been processed, so the absence of its override is deterministic.
func TestThermostatBoostSuppressesOverride(t *testing.T) {
	reg, kv, global, tracker := startThermostat(t)
	ctx := context.Background()

	if err := kv.Set(ctx, "boost:bedroom", map[string]any{
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

	reg.Dispatch(climateChange("climate.bedroom", 18, 22))        // must be suppressed (boost)
	reg.Dispatch(climateChange("climate.childrens_room", 18, 22)) // barrier: must create override

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := overrideTemp(t, kv, "childrens"); ok {
			if _, ok := overrideTemp(t, kv, "bedroom"); ok {
				t.Fatal("boost did not suppress the manual override")
			}
			return // barrier processed and bedroom has no override
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("barrier override (childrens) never appeared")
}
