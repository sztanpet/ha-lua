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

type enhancedFixture struct {
	reg     *Registry
	global  *store.GlobalStore
	tracker *state.Tracker
	ctx     context.Context
	t       *testing.T

	mu    sync.Mutex
	calls []svcCall
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

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	f := &enhancedFixture{reg: reg, global: global, tracker: tracker, t: t}
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
		NewKV:       func(id string) *store.Store { return store.New(writeDB, readDB, id) },
		CallService: callService,
		SetState:    func(context.Context, string, string, jsontext.Value) (bool, error) { return true, nil },
		RemoveState: func(context.Context, string) error { return nil },
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

// seedClimate writes a climate entity into the state mirror as heat mode.
func (f *enhancedFixture) seedClimate(entity, attrs string) {
	f.t.Helper()
	if err := f.tracker.Seed(f.ctx, []ha.StateData{
		{EntityID: entity, State: "heat", Attributes: jsontext.Value(attrs)},
	}); err != nil {
		f.t.Fatal(err)
	}
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
