package lua

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	kv      *store.Store
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
	for _, lib := range []string{"control.lua", "schedule.lua", "card.lua", "climate.lua", "overshoot.lua"} {
		copyRepoFile(t, filepath.Join(repoScriptsDir, "lib", lib), filepath.Join(libDir, lib))
	}
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

	f := &enhancedFixture{
		reg:     reg,
		router:  router,
		global:  global,
		kv:      store.New(writeDB, readDB, "enhanced_climate"),
		tracker: tracker,
		t:       t,
	}
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
		LogsRoot:    openTestRoot(t, t.TempDir()),
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
	for _, v := range slices.Backward(f.publish) {
		if v.entityID != entityID {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(v.attrs, &m); err != nil {
			return v.state, nil
		}
		return v.state, m
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
	return slices.Contains(f.removed, entityID)
}

// companionWrites counts how many set_state calls targeted entityID.
func (f *enhancedFixture) companionWrites(entityID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.publish {
		if p.entityID == entityID {
			n++
		}
	}
	return n
}

// waitWrites blocks until at least want set_state calls have targeted entityID.
func (f *enhancedFixture) waitWrites(entityID string, want int, desc string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.companionWrites(entityID) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timeout waiting for %d writes to %s (%s); got %d", want, entityID, desc, f.companionWrites(entityID))
}

// seedClimate upserts a climate entity into the state mirror as heat mode.
// Upsert via HandleStateChanged, not Seed: Seed carries full-snapshot
// semantics (entities absent from the batch are deleted), so seeding
// one entity at a time would evict the previously seeded ones.
func (f *enhancedFixture) seedClimate(entity, attrs string) {
	f.t.Helper()
	payload := `{"entity_id":"` + entity + `","new_state":{"entity_id":"` + entity +
		`","state":"heat","attributes":` + attrs + `}}`
	if err := f.tracker.HandleStateChanged(f.ctx, jsontext.Value(payload)); err != nil {
		f.t.Fatal(err)
	}
}

// pushClimate is seedClimate plus the dispatch: the mirror is applied first and
// the script then sees the event, in production's order. old is the previous
// attribute JSON, since a handler judging a transition needs both sides.
func (f *enhancedFixture) pushClimate(entity, old, attrs string) {
	f.t.Helper()
	payload := jsontext.Value(`{"entity_id":"` + entity + `","old_state":{"entity_id":"` + entity +
		`","state":"heat","attributes":` + old + `},"new_state":{"entity_id":"` + entity +
		`","state":"heat","attributes":` + attrs + `}}`)
	if err := f.tracker.HandleStateChanged(f.ctx, payload); err != nil {
		f.t.Fatal(err)
	}
	f.reg.Dispatch(ha.Event{Type: "state_changed", Data: payload})
}

// setWindow upserts a binary sensor state into the mirror and dispatches its
// state-change event, exactly as a real window opening/closing would arrive.
func (f *enhancedFixture) setWindow(sensor, st string) {
	f.t.Helper()
	payload := jsontext.Value(`{"entity_id":"` + sensor + `","new_state":{"entity_id":"` + sensor +
		`","state":"` + st + `","attributes":{}}}`)
	if err := f.tracker.HandleStateChanged(f.ctx, payload); err != nil {
		f.t.Fatal(err)
	}
	f.reg.Dispatch(ha.Event{Type: "state_changed", Data: payload})
}

// setSensor upserts a plain numeric sensor into the mirror (no dispatch: these
// are read on demand by the control tick, not subscribed to).
func (f *enhancedFixture) setSensor(entity, value string) {
	f.t.Helper()
	payload := jsontext.Value(`{"entity_id":"` + entity + `","new_state":{"entity_id":"` + entity +
		`","state":"` + value + `","attributes":{}}}`)
	if err := f.tracker.HandleStateChanged(f.ctx, payload); err != nil {
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

// storeNumber reads one of the script's own KV values as a number, failing when
// it is absent or not numeric.
func (f *enhancedFixture) storeNumber(key string) float64 {
	f.t.Helper()
	v, err := f.kv.Get(f.ctx, key)
	if err != nil {
		f.t.Fatalf("read %s: %v", key, err)
	}
	n, ok := v.(float64)
	if !ok {
		f.t.Fatalf("%s = %#v, want a number", key, v)
	}
	return n
}

// setStoreNumber writes one of the script's own KV values, so a test can force
// two keys the script normally keeps equal apart.
func (f *enhancedFixture) setStoreNumber(key string, value float64) {
	f.t.Helper()
	f.setStore(key, value)
}

// setStore seeds one of the script's own KV values, standing in for state the
// script would otherwise take days to accumulate.
func (f *enhancedFixture) setStore(key string, value any) {
	f.t.Helper()
	if err := f.kv.Set(f.ctx, key, value); err != nil {
		f.t.Fatalf("write %s: %v", key, err)
	}
}

// storeMap reads one of the script's own KV values as a table, or nil.
func (f *enhancedFixture) storeMap(key string) map[string]any {
	f.t.Helper()
	v, err := f.kv.Get(f.ctx, key)
	if err != nil {
		f.t.Fatalf("read %s: %v", key, err)
	}
	m, _ := v.(map[string]any)
	return m
}

// overshootJournal reads the episode journal, newest last.
func (f *enhancedFixture) overshootJournal(climate string) []map[string]any {
	f.t.Helper()
	v, err := f.kv.Get(f.ctx, "overshoot_journal:"+climate)
	if err != nil {
		f.t.Fatalf("read journal: %v", err)
	}
	rows, _ := v.([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// waitJournal polls the journal until it holds at least n rows.
func (f *enhancedFixture) waitJournal(climate string, n int, desc string) []map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows := f.overshootJournal(climate); len(rows) >= n {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timeout waiting for %d journal row(s) for %s (%s); got %+v",
		n, climate, desc, f.overshootJournal(climate))
	return nil
}

// waitEpisode polls for a live overshoot episode on the climate.
func (f *enhancedFixture) waitEpisode(climate, desc string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ep := f.storeMap("overshoot_episode:" + climate); ep != nil {
			return ep
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timeout waiting for an episode on %s (%s)", climate, desc)
	return nil
}

// TestEnhancedClimateOvershootOpensOnHeating pins the second trigger of spec
// §5: a hold whose request never moves still opens an episode when the device
// reports its relay closing. This is the case the children's room lives in —
// no schedule, one temperature all day, overheating on every cycle — and the
// one the first version could not see at all, because it only watched the
// request.
func TestEnhancedClimateOvershootOpensOnHeating(t *testing.T) {
	f := newEnhancedFixture(t)
	// The room sits ABOVE the request, so establishing the hold opens nothing:
	// the request changed, but there is nothing to climb.
	f.seedClimate("climate.lr", `{"current_temperature":21.4,"temperature":21,"min_temp":7,"max_temp":35,"hvac_action":"idle"}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.setStore("overshoot_k:climate.lr", map[string]any{"base": 0.8, "slope": 0.0})
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitCompanion("sensor.ha_lua_enhanced_climate_lr", func(_ string, attrs map[string]any) bool {
		return attrs["controlled"] == true
	}, "the hold is established")
	time.Sleep(100 * time.Millisecond)
	if ep := f.storeMap("overshoot_episode:climate.lr"); ep != nil {
		t.Fatalf("an episode opened with the room above the request: %+v", ep)
	}

	// The room drifts under the deadband and the relay closes. The request is
	// still 21 — nothing about it changed — and that alone must open the episode.
	f.pushClimate("climate.lr",
		`{"current_temperature":20.6,"temperature":21,"min_temp":7,"max_temp":35,"hvac_action":"idle"}`,
		`{"current_temperature":20.6,"temperature":21,"min_temp":7,"max_temp":35,"hvac_action":"heating"}`)
	ep := f.waitEpisode("climate.lr", "the relay closing opens a cycle's episode")
	if rise, _ := ep["rise"].(float64); math.Abs(rise-0.4) > 1e-9 {
		t.Errorf("rise = %v, want 0.4 (the deadband)", ep["rise"])
	}
	// The floor is what a cycle gets: base 0.8 on a 0.4 rise is a cut of 0.8.
	if commanded, _ := ep["commanded"].(float64); math.Abs(commanded-20.2) > 1e-9 {
		t.Errorf("commanded = %v, want 20.2 (request minus the floor)", ep["commanded"])
	}
	if ep["applied"] != 21.0 {
		t.Errorf("applied = %v, want 21: observe-only still writes the request", ep["applied"])
	}
	// The relay that opened it is on record from the open, so a cycle that cuts
	// off before the first tick cannot be discarded as never having heated.
	if ep["heated"] != true {
		t.Errorf("heated = %v at the open, want true", ep["heated"])
	}
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

// TestEnhancedClimateManualDetectionReadsWritten pins the manual-change detector
// onto `written` rather than `desired` (overshoot-spec.md §7). The two are equal
// whenever no correction is active, so they are forced apart here: with the
// request at 21 and the commanded value at 19.8, a dial reading 19.8 is the
// controller's own write and must NOT latch a manual hold, while 21 — the
// requested value the detector used to compare against — must.
func TestEnhancedClimateManualDetectionReadsWritten(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "schedule 21 establishes both setpoints")

	// A correction cutting this warmup short would leave exactly this split.
	f.setStoreNumber("written:climate.lr", 19.8)

	f.reg.Dispatch(ha.Event{
		Type: "state_changed",
		Data: jsontext.Value(`{"entity_id":"climate.lr","new_state":{"entity_id":"climate.lr",` +
			`"state":"heat","attributes":{"temperature":19.8,"min_temp":7,"max_temp":35}}}`),
	})
	time.Sleep(200 * time.Millisecond) // the rejection is a non-event; give it room
	if got := f.storeNumber("desired:climate.lr"); got != 21 {
		t.Fatalf("desired = %v after the commanded value appeared on the dial, want 21 (a manual hold latched on our own write)", got)
	}
	if temps := f.setTemps(); slices.Contains(temps, 19.8) {
		t.Fatalf("set_temperature calls %v include the commanded value, so the detector read our own write as a dial change", temps)
	}

	// The requested value, by contrast, is a real dial change now.
	f.reg.Dispatch(ha.Event{
		Type: "state_changed",
		Data: jsontext.Value(`{"entity_id":"climate.lr","new_state":{"entity_id":"climate.lr",` +
			`"state":"heat","attributes":{"temperature":21,"min_temp":7,"max_temp":35}}}`),
	})
	f.waitSetTemp(21, "dialing to the requested value latches a manual hold")
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

// TestEnhancedClimateConfigureStoresRadiator pins that radiator_entity reaches
// the daemon. It used to be display-only card config, deliberately kept out of
// the card's configHash so a cosmetic change could not re-send configure. The
// overshoot journal records the radiator temperature with every episode, so the
// daemon now has to know which sensor it is — and a change to it is a real
// config change, not a cosmetic one.
func TestEnhancedClimateConfigureStoresRadiator(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","radiator_entity":"sensor.lr_rad"}`)
	f.waitRegistry(func(m map[string]any) bool {
		cfg, _ := m["climate.lr"].(map[string]any)
		return cfg != nil && cfg["radiator_entity"] == "sensor.lr_rad"
	}, "radiator_entity stored")

	// Changing only the radiator is a real config change, so it must land.
	f.fireCommand("configure", `{"climate_entity":"climate.lr","radiator_entity":"sensor.other_rad"}`)
	f.waitRegistry(func(m map[string]any) bool {
		cfg, _ := m["climate.lr"].(map[string]any)
		return cfg != nil && cfg["radiator_entity"] == "sensor.other_rad"
	}, "a radiator-only change updates the registry")

	// Omitting it is "no radiator", not a type error further down.
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.waitRegistry(func(m map[string]any) bool {
		cfg, _ := m["climate.lr"].(map[string]any)
		return cfg != nil && cfg["radiator_entity"] == ""
	}, "an absent radiator_entity normalises to empty")
}

// TestEnhancedClimateCompanionSplitsRequestedAndCommanded pins the companion's
// two setpoints apart (overshoot-spec.md §8): `state` is what the user asked
// for, the `commanded` attribute is what the device actually carries. They must
// not collapse into one number, or a corrected warmup would report the reduced
// setpoint under a name that says "requested".
//
// They are forced apart the only way that needs no correction: the fixture's
// call_service is a capture, so the schedule requests 21 while the entity's own
// temperature attribute stays at the seeded 18.
func TestEnhancedClimateCompanionSplitsRequestedAndCommanded(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "schedule requests 21")

	f.waitCompanion("sensor.ha_lua_enhanced_climate_lr", func(state string, attrs map[string]any) bool {
		return state == "21" && attrs["commanded"] == 18.0
	}, "state 21 (requested) with commanded 18 (what the device carries)")
}

// TestEnhancedClimateOvershootObserveOnlyWrites pins the safety gate
// (overshoot-spec.md §9.4): observe-only defaults ON, so a seeded, converged k
// still writes the UNCORRECTED request. The episode is opened and recorded
// either way — that is the point of the mode.
func TestEnhancedClimateOvershootObserveOnlyWrites(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.setStore("overshoot_k:climate.lr", map[string]any{"base": 0.0, "slope": 0.4}) // would cut 1.2° off a 3° rise

	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "observe-only writes the request, not the corrected value")
	if temps := f.setTemps(); slices.Contains(temps, 19.8) {
		t.Fatalf("set_temperature calls %v include the corrected value while observing", temps)
	}

	// Recorded regardless: a week of what it WOULD have done is the whole
	// purpose of shipping this way.
	ep := f.storeMap("overshoot_episode:climate.lr")
	if ep == nil {
		t.Fatal("no episode opened, so observe-only recorded nothing to judge")
	}
	if ep["offset"] != 1.2000000000000002 && ep["offset"] != 1.2 {
		t.Fatalf("episode offset = %v, want 1.2 (k 0.4 x rise 3)", ep["offset"])
	}
	if ep["applied"] != 21.0 {
		t.Fatalf("episode applied = %v, want 21 (the uncorrected request)", ep["applied"])
	}
	if ep["commanded"] != 19.8 {
		t.Fatalf("episode commanded = %v, want 19.8 (what it would have written)", ep["commanded"])
	}
}

// TestEnhancedClimateOvershootCorrects takes one climate out of observe-only and
// checks the correction reaches the device: a 18->21 warmup with k=0.4 is
// commanded to 19.8, while the REQUEST stays 21 for everything that displays
// intent (§5, §8).
func TestEnhancedClimateOvershootCorrects(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.setStore("overshoot_k:climate.lr", map[string]any{"base": 0.0, "slope": 0.4})
	f.setStore("overshoot_observe:climate.lr", false)

	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(19.8, "the corrected cutoff is what the device is commanded to")

	if got := f.storeNumber("desired:climate.lr"); got != 21 {
		t.Fatalf("desired = %v, want 21: the request must survive the correction", got)
	}
	if got := f.storeNumber("written:climate.lr"); got != 19.8 {
		t.Fatalf("written = %v, want 19.8", got)
	}
}

// TestEnhancedClimateOvershootOffsetIsLatched is the one the design calls the
// easiest thing to get wrong (§5): the offset is fixed when the episode opens
// and must NOT be recomputed as the room warms. Recomputed, the commanded value
// would climb with the shrinking rise and converge on the request without ever
// cutting early — the correction would silently do nothing.
func TestEnhancedClimateOvershootOffsetIsLatched(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.setStore("overshoot_k:climate.lr", map[string]any{"base": 0.0, "slope": 0.4})
	f.setStore("overshoot_observe:climate.lr", false)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(19.8, "episode opens and latches a 1.2° offset")

	// The room climbs to 20 and the device now carries the corrected setpoint.
	// A recomputed offset would be 0.4 x 1 = 0.4, i.e. a command of 20.6.
	f.seedClimate("climate.lr", `{"current_temperature":20,"temperature":19.8,"min_temp":7,"max_temp":35}`)
	f.fireCommand("settings", `{"climate_entity":"climate.lr","override_temp":24}`)

	time.Sleep(200 * time.Millisecond) // the re-latch would be a non-event
	if got := f.storeNumber("written:climate.lr"); got != 19.8 {
		t.Fatalf("written = %v after the room warmed, want 19.8: the offset was recomputed instead of latched", got)
	}
	if temps := f.setTemps(); slices.Contains(temps, 20.6) {
		t.Fatalf("set_temperature calls %v show a recomputed offset", temps)
	}
}

// TestEnhancedClimateOvershootDiscardsOnWindow pins §9.1's central rule: a
// discarded episode is a record with a reason, never a bare return. A window
// opening mid-warmup makes the thermal picture meaningless, so the episode is
// abandoned — but it lands in the journal, because a learner that silently
// discards every episode is indistinguishable from one that has converged.
func TestEnhancedClimateOvershootDiscardsOnWindow(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.setWindow("binary_sensor.w1", "off")
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1"]}`)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "episode opens on the warmup")

	f.setWindow("binary_sensor.w1", "on")

	rows := f.waitJournal("climate.lr", 1, "the window discard is journaled")
	last := rows[len(rows)-1]
	if last["outcome"] != "discarded" {
		t.Fatalf("outcome = %v, want discarded", last["outcome"])
	}
	if last["reason"] != "window_open" {
		t.Fatalf("reason = %v, want window_open", last["reason"])
	}
	// Both the inputs and the resulting action, so the episode can be re-judged
	// later without the surrounding state.
	if last["requested"] != 21.0 || last["rise"] != 3.0 {
		t.Fatalf("journal row lost its deciding inputs: %+v", last)
	}
	if f.storeMap("overshoot_episode:climate.lr") != nil {
		t.Fatal("an invalidated episode must close immediately, not limp on")
	}
	// Nothing was learned from it: a discard writes no sample at all.
	if got, err := f.kv.Get(f.ctx, "overshoot_samples:climate.lr"); err != nil {
		t.Fatalf("read samples: %v", err)
	} else if got != nil && got != 0.0 {
		t.Fatalf("samples = %v after a discard, want unset or 0", got)
	}
}

// TestEnhancedClimateRemovalPage drives the Ingress removal page: /api/list
// reports the registry, GET / serves the HTML, and POST /api/remove
// deprovisions a climate (removing its companion) while a bad body is rejected.
func TestEnhancedClimateRemovalPage(t *testing.T) {
	f := newEnhancedFixture(t)
	waitRouteID(t, f.router, "enhanced_climate", "GET", "/api/list")

	f.seedClimate("climate.lr", `{"friendly_name":"Living Room"}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1"]}`)
	f.waitRegistry(func(m map[string]any) bool { return m != nil && m["climate.lr"] != nil }, "lr configured")

	// /api/list reports the climate with its friendly name.
	rec := doReqID(f.router, "enhanced_climate", "GET", "/api/list", "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/list status %d", rec.Code)
	}
	var listed struct {
		Climates []struct {
			ClimateEntity string   `json:"climate_entity"`
			Name          string   `json:"name"`
			WindowSensors []string `json:"window_sensors"`
			Overshoot     *struct {
				K           struct{ Base, Slope float64 } `json:"k"`
				Samples     int                           `json:"samples"`
				ObserveOnly bool                          `json:"observe_only"`
			} `json:"overshoot"`
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
	// The page renders the learner summary straight off this row, so it has to
	// be here — and a fresh climate must read as observing, not as armed.
	if listed.Climates[0].Overshoot == nil {
		t.Fatal("/api/list carries no overshoot summary for the page to render")
	}
	if !listed.Climates[0].Overshoot.ObserveOnly {
		t.Error("a newly configured climate is not in observe-only")
	}

	// GET / serves the self-contained HTML page.
	rec = doReqID(f.router, "enhanced_climate", "GET", "/", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("GET / did not return the HTML page")
	}

	// POST /api/remove deprovisions it (synchronous handler) and removes the
	// companion.
	rec = doReqID(f.router, "enhanced_climate", "POST", "/api/remove", `{"climate_entity":"climate.lr"}`)
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
	rec = doReqID(f.router, "enhanced_climate", "POST", "/api/remove", `not json`)
	if rec.Code != 400 {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}
}

// TestEnhancedClimateOvershootRecordsConditions pins the two columns that exist
// purely to be analysed later (overshoot-spec.md §12): the outdoor temperature
// and the radiator temperature an episode ran under. Nothing reads them — one
// coefficient per climate assumes the plant gain is roughly constant, and these
// are what will eventually say whether that assumption holds. A journal without
// them makes the question permanently unanswerable.
func TestEnhancedClimateOvershootRecordsConditions(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.setSensor("sensor.lr_rad", "28")
	f.setSensor("sensor.outside", "4.5")
	f.setWindow("binary_sensor.w1", "off")
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1"],`+
		`"radiator_entity":"sensor.lr_rad","outdoor_entity":"sensor.outside"}`)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "episode opens with the radiator cold")

	ep := f.storeMap("overshoot_episode:climate.lr")
	if ep == nil {
		t.Fatal("no episode opened")
	}
	if ep["outdoor_at_open"] != 4.5 {
		t.Errorf("outdoor_at_open = %v, want 4.5", ep["outdoor_at_open"])
	}
	if ep["radiator_at_open"] != 28.0 {
		t.Errorf("radiator_at_open = %v, want 28", ep["radiator_at_open"])
	}

	// The radiator heats up, the window opens, the episode is discarded — and
	// the conditions travel into the journal row with everything else.
	f.setSensor("sensor.lr_rad", "61")
	f.setWindow("binary_sensor.w1", "on")
	rows := f.waitJournal("climate.lr", 1, "the discard carries the conditions")
	last := rows[len(rows)-1]
	if last["outdoor_at_open"] != 4.5 || last["radiator_at_open"] != 28.0 {
		t.Errorf("journal row lost the conditions: %+v", last)
	}
}

// TestEnhancedClimateOvershootAPI covers §9.5/§9.6: the learner's state is
// curl-able, and recovering from a bad k is neither `sqlite3 /data/ha-lua.db`
// nor a restart. The reset and observe writes go through both channels the user
// has — the Ingress endpoints and the card's command event.
func TestEnhancedClimateOvershootAPI(t *testing.T) {
	f := newEnhancedFixture(t)
	waitRouteID(t, f.router, "enhanced_climate", "GET", "/api/overshoot")

	f.seedClimate("climate.lr", `{"friendly_name":"Living Room","current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.waitRegistry(func(m map[string]any) bool { return m != nil && m["climate.lr"] != nil }, "lr configured")
	f.setStore("overshoot_k:climate.lr", map[string]any{"base": 0.0, "slope": 0.4})
	f.setStoreNumber("overshoot_samples:climate.lr", 6)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "observe-only warmup opens an episode")

	type report struct {
		ClimateEntity string                        `json:"climate_entity"`
		K             struct{ Base, Slope float64 } `json:"k"`
		Samples       int                           `json:"samples"`
		ObserveOnly   bool                          `json:"observe_only"`
		Episode       *struct {
			Requested float64 `json:"requested"`
			Commanded float64 `json:"commanded"`
			Offset    float64 `json:"offset"`
		} `json:"episode"`
	}
	get := func() report {
		t.Helper()
		rec := doReqID(f.router, "enhanced_climate", "GET", "/api/overshoot?climate=climate.lr", "")
		if rec.Code != 200 {
			t.Fatalf("GET /api/overshoot status %d body %q", rec.Code, rec.Body.String())
		}
		var r report
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return r
	}

	// The live episode is reachable while it is still running, which is the only
	// way to answer "why is it commanding that" before the journal exists.
	got := get()
	if got.K.Slope != 0.4 || got.K.Base != 0 || got.Samples != 6 || !got.ObserveOnly {
		t.Fatalf("report = %+v, want slope 0.4 over 6 samples, observing", got)
	}
	if got.Episode == nil || got.Episode.Commanded != 19.8 {
		t.Fatalf("live episode = %+v, want commanded 19.8", got.Episode)
	}

	// Taking it out of observe-only over HTTP closes the running episode rather
	// than judging it under a setting it did not latch.
	rec := doReqID(f.router, "enhanced_climate", "POST", "/api/overshoot/observe",
		`{"climate_entity":"climate.lr","observe_only":false}`)
	if rec.Code != 200 {
		t.Fatalf("POST observe status %d body %q", rec.Code, rec.Body.String())
	}
	if got := get(); got.ObserveOnly {
		t.Fatal("observe_only still set after the write")
	}
	rows := f.waitJournal("climate.lr", 1, "the latched episode is closed, not silently dropped")
	if last := rows[len(rows)-1]; last["reason"] != "observe_changed" {
		t.Fatalf("reason = %v, want observe_changed", last["reason"])
	}

	// Reset zeroes k and clears the journal, with no restart and no sqlite3.
	rec = doReqID(f.router, "enhanced_climate", "POST", "/api/overshoot/reset",
		`{"climate_entity":"climate.lr"}`)
	if rec.Code != 200 {
		t.Fatalf("POST reset status %d body %q", rec.Code, rec.Body.String())
	}
	if got := get(); got.K.Base != 0 || got.K.Slope != 0 || got.Samples != 0 {
		t.Fatalf("after reset %+v, want k 0 over 0 samples", got)
	}
	if rows := f.overshootJournal("climate.lr"); len(rows) != 0 {
		t.Fatalf("journal survived the reset: %+v", rows)
	}

	// The card reaches the same two writes over its own channel.
	f.fireCommand("overshoot", `{"climate_entity":"climate.lr","observe_only":true}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !get().ObserveOnly {
		time.Sleep(10 * time.Millisecond)
	}
	if !get().ObserveOnly {
		t.Fatal("the card's overshoot command did not set observe_only")
	}

	// An unknown climate is a 404 on every route, not a silent success.
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/overshoot?climate=climate.nope", ""},
		{"POST", "/api/overshoot/reset", `{"climate_entity":"climate.nope"}`},
		{"POST", "/api/overshoot/observe", `{"climate_entity":"climate.nope","observe_only":true}`},
	} {
		if rec := doReqID(f.router, "enhanced_climate", tc.method, tc.path, tc.body); rec.Code != 404 {
			t.Errorf("%s %s status = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
	// A non-boolean observe_only is a 400, so a typo cannot quietly arm the
	// correction on a child's bedroom.
	if rec := doReqID(f.router, "enhanced_climate", "POST", "/api/overshoot/observe",
		`{"climate_entity":"climate.lr","observe_only":"false"}`); rec.Code != 400 {
		t.Errorf("string observe_only status = %d, want 400", rec.Code)
	}
}

// TestEnhancedClimateCompanionDedupsUnchanged verifies the controller does not
// rewrite a companion whose state+attributes are unchanged: an identical
// re-apply issues no set_state, while a real change still publishes. This is
// what keeps the 1-minute tick (and every event-driven re-apply) from spamming
// HA with no-op state_changed writes.
func TestEnhancedClimateCompanionDedupsUnchanged(t *testing.T) {
	f := newEnhancedFixture(t)
	const companion = "sensor.ha_lua_enhanced_climate_lr"

	f.seedClimate("climate.lr", `{"temperature":20,"min_temp":5,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.w1"]}`)
	f.waitRegistry(func(m map[string]any) bool {
		return m != nil && m["climate.lr"] != nil
	}, "configure climate.lr")

	// Seed the window closed and wait until the companion reflects it; this is
	// the steady state we then re-apply without change.
	f.setWindow("binary_sensor.w1", "off")
	f.waitCompanion(companion, func(_ string, attrs map[string]any) bool {
		w, _ := attrs["window"].(map[string]any)
		return w != nil && w["open"] == false
	}, "window closed")

	baseline := f.companionWrites(companion)

	// Re-apply with the window still closed: identical payload, so no write.
	f.setWindow("binary_sensor.w1", "off")
	time.Sleep(200 * time.Millisecond) // let the re-apply run (asserting a non-event)
	if got := f.companionWrites(companion); got != baseline {
		t.Errorf("unchanged re-apply wrote %d extra companion update(s); want 0", got-baseline)
	}

	// A real change (window opens) must still publish.
	f.setWindow("binary_sensor.w1", "on")
	f.waitWrites(companion, baseline+1, "publish after window opens")
}

// TestEnhancedClimateConfigureRepublishesCompanion verifies a configure with an
// unchanged config still re-publishes the companion. The card sends configure
// only when it cannot see a matching companion (e.g. HA dropped the
// non-integration entity on restart); if the daemon no-oped on an unchanged
// config the companion would never reappear and the card would retry forever.
func TestEnhancedClimateConfigureRepublishesCompanion(t *testing.T) {
	f := newEnhancedFixture(t)
	const companion = "sensor.ha_lua_enhanced_climate_lr"

	f.seedClimate("climate.lr", `{"temperature":20,"min_temp":5,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":[],"presets":[]}`)
	f.waitWrites(companion, 1, "initial configure publish")

	before := f.companionWrites(companion)
	// Identical config: nothing in the registry changes, but the companion must
	// still be re-published (the card is asking because it vanished).
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":[],"presets":[]}`)
	f.waitWrites(companion, before+1, "republish on unchanged configure")
}

// TestEnhancedClimateOverrideRestoresSetpoint: a boost on a climate with no
// schedule and no manual hold has nothing underneath it to take over when it
// ends, so it must put back the setpoint it found — otherwise the boost
// temperature is where the dial stays for good.
func TestEnhancedClimateOverrideRestoresSetpoint(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":22,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)

	f.fireCommand("override", `{"climate_entity":"climate.lr","minutes":10}`)
	f.waitSetTemp(23, "boost to the default override temp")
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":23,"min_temp":7,"max_temp":35}`)

	// Pressing a preset again extends the boost; it must not snapshot 23 as the
	// way back.
	f.fireCommand("override", `{"climate_entity":"climate.lr","minutes":10}`)

	f.fireCommand("override", `{"climate_entity":"climate.lr","cancel":true}`)
	f.waitSetTemp(22, "the pre-boost setpoint is restored")

	// One-shot: the climate is uncontrolled now, so a later pass must not write
	// 22 again over whatever the user has since dialled in.
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":24,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr","window_sensors":["binary_sensor.x"]}`)
	f.waitCompanion("sensor.ha_lua_enhanced_climate_lr", func(_ string, attrs map[string]any) bool {
		sensors, _ := attrs["window"].(map[string]any)["sensors"].([]any)
		return len(sensors) == 1
	}, "re-apply after the restore")
	if temps := f.setTemps(); temps[len(temps)-1] != 22 {
		t.Fatalf("setpoint written after the one-shot restore: %v", temps)
	}
}

// TestEnhancedClimateOverrideKeepsSchedule: with a schedule underneath, the
// boost ending falls back to the schedule, and the pre-boost snapshot must be
// dropped rather than fight it.
func TestEnhancedClimateOverrideKeepsSchedule(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":18,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)
	f.fireCommand("schedule", `{"climate_entity":"climate.lr","schedule":`+allDaySchedule("21")+`}`)
	f.waitSetTemp(21, "schedule 21")
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":21,"min_temp":7,"max_temp":35}`)

	f.fireCommand("override", `{"climate_entity":"climate.lr","minutes":10}`)
	f.waitSetTemp(23, "boost to 23")
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":23,"min_temp":7,"max_temp":35}`)

	f.fireCommand("override", `{"climate_entity":"climate.lr","cancel":true}`)
	f.waitSetTemp(21, "schedule takes back over")
}

// TestEnhancedClimateOverrideExpires: an override must clear itself the moment
// it ends. Leaving that to the 1-minute control tick left the card's countdown
// frozen at 00:00 — and the boost apparently still running — for up to a minute.
func TestEnhancedClimateOverrideExpires(t *testing.T) {
	f := newEnhancedFixture(t)
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":22,"min_temp":7,"max_temp":35}`)
	f.fireCommand("configure", `{"climate_entity":"climate.lr"}`)

	f.fireCommand("override", `{"climate_entity":"climate.lr","minutes":0.02}`) // 1.2s
	f.waitSetTemp(23, "boost to the default override temp")
	f.seedClimate("climate.lr", `{"current_temperature":18,"temperature":23,"min_temp":7,"max_temp":35}`)

	// No tick, no command, no state change: the boost's own timer has to be
	// what puts the companion back to inactive.
	f.waitCompanion("sensor.ha_lua_enhanced_climate_lr", func(_ string, attrs map[string]any) bool {
		o, _ := attrs["override"].(map[string]any)
		return o != nil && o["active"] == false
	}, "the override clears itself when it ends")
	f.waitSetTemp(22, "the pre-boost setpoint comes back on expiry")
}
