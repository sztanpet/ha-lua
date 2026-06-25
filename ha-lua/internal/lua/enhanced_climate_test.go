package lua

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// enhancedFixture loads the real enhanced_climate.lua (with its libs, a real
// scheduler, and the Supervisor wiring) and exposes the registry, the global
// store, and a command-firing helper. Mutations arrive as ha_lua_command events
// dispatched through the registry, exactly as the daemon delivers a card's
// event. Companion publish/remove are wired with no-op SetState/RemoveState
// here; the tests that assert on them arrive with that milestone.
type svcCall struct {
	domain, service string
	data            jsontext.Value
}

type stateCall struct {
	entityID, state string
	attrs           jsontext.Value
}

type enhancedFixture struct {
	reg     *Registry
	router  *Router
	global  *store.GlobalStore
	tracker *state.Tracker
	ctx     context.Context
	t       *testing.T

	mu      sync.Mutex
	calls   []svcCall
	publish []stateCall
	removed []string
}

func newEnhancedFixture(t *testing.T) *enhancedFixture {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "control.lua"), filepath.Join(libDir, "control.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "schedule.lua"), filepath.Join(libDir, "schedule.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", "card.lua"), filepath.Join(libDir, "card.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "enhanced_climate.lua"), filepath.Join(dir, "enhanced_climate.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "enhanced_climate.html"), filepath.Join(dir, "enhanced_climate.html"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	router := NewRouter(reg)
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	f := &enhancedFixture{reg: reg, router: router, global: global, tracker: tracker, t: t}
	callService := func(_ context.Context, domain, service string, data jsontext.Value) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, svcCall{domain, service, data})
		return nil
	}

	sup := NewSupervisor(reg, dir, Deps{
		Tracker:     tracker,
		Scheduler:   sched,
		Global:      global,
		Root:        openTestRoot(t, dir),
		Router:      router,
		NewKV:       func(id string) *store.Store { return store.New(writeDB, readDB, id) },
		CallService: callService,
		SetState: func(_ context.Context, entityID, st string, attrs jsontext.Value) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.publish = append(f.publish, stateCall{entityID, st, attrs})
			return true, nil
		},
		RemoveState: func(_ context.Context, entityID string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.removed = append(f.removed, entityID)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	f.ctx = ctx
	t.Cleanup(func() { cancel(); sup.Wait() })
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}

	r := reg.Get("enhanced_climate")
	if r == nil {
		t.Fatal("enhanced_climate runner not registered")
	}
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("enhanced_climate.lua did not finish loading")
	}

	return f
}

// setTemps returns the temperatures passed to every captured
// climate.set_temperature call, in order.
func (f *enhancedFixture) setTemps() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var temps []float64
	for _, c := range f.calls {
		if c.domain != "climate" || c.service != "set_temperature" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(c.data, &m); err != nil {
			continue
		}
		if temp, ok := m["temperature"].(float64); ok {
			temps = append(temps, temp)
		}
	}
	return temps
}

// waitSetTemp waits until the most recent set_temperature is want, or fails.
func (f *enhancedFixture) waitSetTemp(want float64, desc string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		temps := f.setTemps()
		if n := len(temps); n > 0 && temps[n-1] == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timeout waiting for set_temperature %v (%s); got %v", want, desc, f.setTemps())
}

// fireCommand dispatches an ha_lua_command for enhanced_climate, mirroring a
// card firing the event. data is a raw JSON object.
func (f *enhancedFixture) fireCommand(action, data string) {
	f.reg.Dispatch(ha.Event{
		Type: "ha_lua_command",
		Data: jsontext.Value(`{"script":"enhanced_climate","action":"` + action + `","data":` + data + `}`),
	})
}

// lastCompanion returns the most recent set_state (state, attributes) for an
// entity, or "", nil if none.
func (f *enhancedFixture) lastCompanion(entityID string) (string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.publish) - 1; i >= 0; i-- {
		if f.publish[i].entityID != entityID {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(f.publish[i].attrs, &m); err != nil {
			return f.publish[i].state, nil
		}
		return f.publish[i].state, m
	}
	return "", nil
}

// waitCompanion waits until the latest companion for entityID satisfies check.
func (f *enhancedFixture) waitCompanion(entityID string, check func(state string, attrs map[string]any) bool, desc string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, attrs := f.lastCompanion(entityID)
		if attrs != nil && check(state, attrs) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, attrs := f.lastCompanion(entityID)
	f.t.Fatalf("timeout waiting for companion %s (%s); last state=%q attrs=%+v", entityID, desc, state, attrs)
}

// removedCompanion reports whether remove_state was called for entityID.
func (f *enhancedFixture) removedCompanion(entityID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.removed {
		if r == entityID {
			return true
		}
	}
	return false
}

// seedClimate writes a climate entity into the state mirror as heat mode.
func (f *enhancedFixture) seedClimate(entity, attrs string) {
	f.t.Helper()
	if err := f.tracker.Seed(f.ctx, []ha.StateData{
		{EntityID: entity, State: "heat", Attributes: jsontext.Value(attrs)},
	}); err != nil {
		f.t.Fatal(err)
	}
}

// setWindow seeds a binary sensor state into the mirror and dispatches its
// state-change event, exactly as a real window opening/closing would arrive.
func (f *enhancedFixture) setWindow(sensor, st string) {
	f.t.Helper()
	if err := f.tracker.Seed(f.ctx, []ha.StateData{
		{EntityID: sensor, State: st, Attributes: jsontext.Value("{}")},
	}); err != nil {
		f.t.Fatal(err)
	}
	f.reg.Dispatch(ha.Event{
		Type: "state_changed",
		Data: jsontext.Value(`{"entity_id":"` + sensor + `","new_state":{"entity_id":"` + sensor +
			`","state":"` + st + `","attributes":{}}}`),
	})
}

// allDaySchedule builds a schedule JSON where every weekday has a single
// 00:00 transition to temp, so schedule.resolve returns temp at any time.
func allDaySchedule(temp string) string {
	var b strings.Builder
	b.WriteByte('{')
	for d := 0; d <= 6; d++ {
		if d > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%d":[{"time":"00:00","temp":%s}]`, d, temp)
	}
	b.WriteByte('}')
	return b.String()
}

// registry reads the current registry map (nil if unset/not-a-map).
func (f *enhancedFixture) registry() map[string]any {
	v, err := f.global.Get(f.ctx, "enhanced_climate:registry")
	if err != nil {
		f.t.Fatalf("read registry: %v", err)
	}
	m, _ := v.(map[string]any)
	return m
}

// waitRegistry polls the registry until check passes, or fails after a timeout.
func (f *enhancedFixture) waitRegistry(check func(map[string]any) bool, desc string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := f.registry()
		if check(m) {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timeout waiting for %s (registry=%+v)", desc, f.registry())
	return nil
}

// TestEnhancedClimateConfigure drives the configure/remove command handlers:
// configure creates a registry entry, a changed config updates it, and remove
// deletes it.
func TestEnhancedClimateConfigure(t *testing.T) {
	f := newEnhancedFixture(t)

	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1"],"presets":[10,30]}`)
	m := f.waitRegistry(func(m map[string]any) bool {
		return m != nil && m["climate.lr"] != nil
	}, "configure to create climate.lr")

	entry, _ := m["climate.lr"].(map[string]any)
	if entry == nil {
		t.Fatalf("climate.lr entry missing: %+v", m)
	}
	if entry["climate_entity"] != "climate.lr" {
		t.Errorf("climate_entity = %v", entry["climate_entity"])
	}
	sensors, _ := entry["window_sensors"].([]any)
	if len(sensors) != 1 || sensors[0] != "binary_sensor.w1" {
		t.Errorf("window_sensors = %v", entry["window_sensors"])
	}

	// A changed config updates the stored list.
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1","binary_sensor.w2"]}`)
	f.waitRegistry(func(m map[string]any) bool {
		entry, _ := m["climate.lr"].(map[string]any)
		if entry == nil {
			return false
		}
		sensors, _ := entry["window_sensors"].([]any)
		return len(sensors) == 2
	}, "configure to update window_sensors")

	// remove deprovisions it.
	f.fireCommand("remove", `{"climate_entity":"climate.lr"}`)
	f.waitRegistry(func(m map[string]any) bool {
		return m == nil || m["climate.lr"] == nil
	}, "remove to delete climate.lr")
}

// TestEnhancedClimateControl drives the control loop through the enhanced-layer
// commands: a schedule resolves and writes the setpoint, the desired is clamped
// to the device range, and override/settings (valid + rejected) behave.
func TestEnhancedClimateControl(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)

	// Schedule resolves to 21 and is written.
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "schedule resolves to 21")

	// A 33 schedule is valid under max 35 and written.
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("33")+`}`)
	f.waitSetTemp(33, "schedule 33 under max 35")

	// Lower the device ceiling to 30; re-applying (via a changed configure) must
	// clamp the 33 schedule down to the device max rather than write a value HA
	// would silently drop.
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":33,"min_temp":7,"max_temp":30}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.x"]}`)
	f.waitSetTemp(30, "desired clamped to max 30")

	// A timed override drives the climate to the default override temp (23).
	f.fireCommand("override", `{"climate_entity":"climate.lr","minutes":30}`)
	f.waitSetTemp(23, "override to default override_temp 23")

	// settings out of range is rejected (override_temp stays 23); a valid value
	// applies immediately because an override is active.
	f.fireCommand("settings", `{"climate_entity":"climate.lr","override_temp":99}`)
	f.fireCommand("settings", `{"climate_entity":"climate.lr","override_temp":25}`)
	f.waitSetTemp(25, "valid override_temp applies under active override")
	for _, temp := range f.setTemps() {
		if temp == 99 {
			t.Fatalf("a rejected out-of-range override_temp was written: %v", f.setTemps())
		}
	}
}

// TestEnhancedClimateManualHold confirms a user changing the climate target
// (different from the published desired) becomes a manual hold that the
// controller then writes.
func TestEnhancedClimateManualHold(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "schedule 21 establishes the published desired")

	// The user dials the climate to 19 — differs from the published 21, so it
	// becomes a manual hold and the controller writes 19.
	f.reg.Dispatch(ha.Event{
		Type: "state_changed",
		Data: jsontext.Value(`{"entity_id":"climate.lr","new_state":{"entity_id":"climate.lr",` +
			`"state":"heat","attributes":{"temperature":19,"min_temp":7,"max_temp":35}}}`),
	})
	f.waitSetTemp(19, "manual hold to the dialed 19")
}

// TestEnhancedClimateWindow confirms window cooperation: any bound window open
// pauses heating to the frost setpoint, and only all-closed restores the
// desired (the multi-sensor any-open/all-closed reduction).
func TestEnhancedClimateWindow(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1","binary_sensor.w2"]}`)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "schedule 21 while windows closed")

	// One window opens -> pause to frost (15).
	f.setWindow("binary_sensor.w1", "on")
	f.waitSetTemp(15, "window open -> frost")

	// It closes again (the other was never open) -> restore the desired.
	f.setWindow("binary_sensor.w1", "off")
	f.waitSetTemp(21, "all closed -> restore desired")

	// Both open -> frost; closing only one must keep it paused (any-open).
	f.setWindow("binary_sensor.w1", "on")
	f.setWindow("binary_sensor.w2", "on")
	f.waitSetTemp(15, "any open -> frost")

	f.setWindow("binary_sensor.w1", "off") // w2 still open
	// FIFO barrier: a configure dispatched after the close is processed only
	// once the close has been, so the assertion below is deterministic.
	f.fireCommand("configure", `{"climate_entity":"climate.barrier"}`)
	f.waitRegistry(func(m map[string]any) bool { return m != nil && m["climate.barrier"] != nil }, "barrier processed")
	if temps := f.setTemps(); temps[len(temps)-1] != 15 {
		t.Fatalf("one of two windows still open must stay paused at frost; last set_temp=%v", temps[len(temps)-1])
	}

	f.setWindow("binary_sensor.w2", "off") // now all closed
	f.waitSetTemp(21, "all closed -> restore desired")
}

// TestEnhancedClimateCompanion confirms the companion sensor is published with
// the right state/attributes through configure / mutation, and removed on
// remove.
func TestEnhancedClimateCompanion(t *testing.T) {
	const companion = "sensor.ha_lua_enhanced_climate_lr"
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"friendly_name":"Living Room","min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1"],"presets":[10,30,60]}`)

	// Configure publishes the companion: no schedule yet -> "off", not controlled.
	f.waitCompanion(companion, func(state string, a map[string]any) bool {
		return state == "off" && a["controlled"] == false
	}, "initial off / not controlled")

	_, attrs := f.lastCompanion(companion)
	if attrs["ha_lua_climate"] != "climate.lr" {
		t.Errorf("ha_lua_climate = %v", attrs["ha_lua_climate"])
	}
	if attrs["friendly_name"] != "Living Room" {
		t.Errorf("friendly_name = %v", attrs["friendly_name"])
	}
	if attrs["unit_of_measurement"] != "°C" || attrs["device_class"] != "temperature" {
		t.Errorf("unit/device_class = %v / %v", attrs["unit_of_measurement"], attrs["device_class"])
	}
	if attrs["removal"] == nil || attrs["removal"] == "" {
		t.Errorf("removal pointer attribute missing")
	}
	win, _ := attrs["window"].(map[string]any)
	if win == nil || win["open"] != false {
		t.Errorf("window block = %v", attrs["window"])
	}
	if sensors, _ := win["sensors"].([]any); len(sensors) != 1 {
		t.Errorf("window.sensors = %v", win["sensors"])
	}
	if presets, _ := attrs["presets"].([]any); len(presets) != 3 {
		t.Errorf("presets = %v", attrs["presets"])
	}
	// override_temp is surfaced (default 23) even with no override active, so the
	// card can show/edit it.
	if attrs["override_temp"] != float64(23) {
		t.Errorf("override_temp = %v, want default 23", attrs["override_temp"])
	}

	// A schedule makes it controlled, with the desired as the state.
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitCompanion(companion, func(state string, a map[string]any) bool {
		return state == "21" && a["controlled"] == true
	}, "controlled at 21")

	// An override is reflected in the override block.
	f.fireCommand("override", `{"climate_entity":"climate.lr","minutes":30}`)
	f.waitCompanion(companion, func(_ string, a map[string]any) bool {
		o, _ := a["override"].(map[string]any)
		return o != nil && o["active"] == true
	}, "override active in companion")

	// remove deletes the companion entity.
	f.fireCommand("remove", `{"climate_entity":"climate.lr"}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !f.removedCompanion(companion) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.removedCompanion(companion) {
		t.Fatalf("remove did not remove_state the companion %s", companion)
	}
}

// TestEnhancedClimateRemovalPage drives the Ingress removal page: /api/list
// reports the registry, GET / serves the HTML, and POST /api/remove
// deprovisions a climate (removing its companion) while a bad body is rejected.
func TestEnhancedClimateRemovalPage(t *testing.T) {
	f := newEnhancedFixture(t)
	waitRoute(t, f.router, "GET", "/api/list")

	f.seedClimate("climate.lr", `{"friendly_name":"Living Room"}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1"]}`)
	f.waitRegistry(func(m map[string]any) bool { return m != nil && m["climate.lr"] != nil }, "lr configured")

	// /api/list reports the climate with its friendly name.
	rec := doReq(f.router, "GET", "/api/list", "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/list status %d", rec.Code)
	}
	var listed struct {
		Climates []struct {
			ClimateEntity string   `json:"climate_entity"`
			Name          string   `json:"name"`
			WindowSensors []string `json:"window_sensors"`
		} `json:"climates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode /api/list %q: %v", rec.Body.String(), err)
	}
	if len(listed.Climates) != 1 || listed.Climates[0].ClimateEntity != "climate.lr" {
		t.Fatalf("unexpected list: %+v", listed.Climates)
	}
	if listed.Climates[0].Name != "Living Room" || len(listed.Climates[0].WindowSensors) != 1 {
		t.Errorf("entry detail wrong: %+v", listed.Climates[0])
	}

	// GET / serves the self-contained HTML page.
	rec = doReq(f.router, "GET", "/", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("GET / did not return the HTML page")
	}

	// POST /api/remove deprovisions it (synchronous handler) and removes the
	// companion.
	rec = doReq(f.router, "POST", "/api/remove", `{"climate_entity":"climate.lr"}`)
	if rec.Code != 200 {
		t.Fatalf("POST /api/remove status %d body %q", rec.Code, rec.Body.String())
	}
	if m := f.registry(); m != nil && m["climate.lr"] != nil {
		t.Errorf("climate.lr still in registry after remove: %+v", m)
	}
	if !f.removedCompanion("sensor.ha_lua_enhanced_climate_lr") {
		t.Errorf("removal page did not remove_state the companion")
	}

	// A malformed body is rejected.
	rec = doReq(f.router, "POST", "/api/remove", `not json`)
	if rec.Code != 400 {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}
}
