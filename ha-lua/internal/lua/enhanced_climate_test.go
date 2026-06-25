package lua

import (
	"context"
	"os"
	"path/filepath"
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
type enhancedFixture struct {
	reg    *Registry
	global *store.GlobalStore
	ctx    context.Context
	t      *testing.T
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

	sup := NewSupervisor(reg, dir, Deps{
		Tracker:     tracker,
		Scheduler:   sched,
		Global:      global,
		Root:        openTestRoot(t, dir),
		NewKV:       func(id string) *store.Store { return store.New(writeDB, readDB, id) },
		CallService: func(context.Context, string, string, jsontext.Value) error { return nil },
		SetState:    func(context.Context, string, string, jsontext.Value) (bool, error) { return true, nil },
		RemoveState: func(context.Context, string) error { return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
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

	return &enhancedFixture{reg: reg, global: global, ctx: ctx, t: t}
}

// fireCommand dispatches an ha_lua_command for enhanced_climate, mirroring a
// card firing the event. data is a raw JSON object.
func (f *enhancedFixture) fireCommand(action, data string) {
	f.reg.Dispatch(ha.Event{
		Type: "ha_lua_command",
		Data: jsontext.Value(`{"script":"enhanced_climate","action":"` + action + `","data":` + data + `}`),
	})
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
