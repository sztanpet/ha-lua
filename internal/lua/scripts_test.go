package lua

import (
	"context"
	"encoding/json"
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
