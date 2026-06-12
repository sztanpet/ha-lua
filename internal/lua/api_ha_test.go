package lua

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-json-experiment/json/jsontext"
	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

func newHALState(t testing.TB) (*lua.LState, *haAPI, *state.Tracker) {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, "test")
	global := store.NewGlobal(writeDB, readDB)

	L := lua.NewState()
	t.Cleanup(L.Close)
	L.SetContext(context.Background())

	runner := &Runner{
		scriptID:  "test",
		scriptDir: t.TempDir(),
		ch:        make(chan Event, 8),
		timerFns:  make(map[string]*lua.LFunction),
		LoadedCh:  make(chan struct{}),
		tracker:   tracker,
		kv:        kv,
		global:    global,
	}
	api := &haAPI{scriptID: "test", tracker: tracker}
	runner.registerHaAPI(L, api)
	registerStoreAPI(L, kv, global)
	return L, api, tracker
}

func TestOnExceptionCalled(t *testing.T) {
	L, api, _ := newHALState(t)

	var caught string
	api.onExceptionFn = L.NewFunction(func(L *lua.LState) int {
		info := L.CheckTable(1)
		caught = luaStrField(info, "error")
		return 0
	})

	fn := L.NewFunction(func(L *lua.LState) int {
		L.RaiseError("test error from callback")
		return 0
	})
	callProtected(L, api, "state_changed", lua.LNil, fn)

	if !strings.Contains(caught, "test error from callback") {
		t.Errorf("exception handler not called or wrong message: %q", caught)
	}
}

func TestExceptionInfoFields(t *testing.T) {
	L, api, _ := newHALState(t)

	var scriptID, callback string
	api.onExceptionFn = L.NewFunction(func(L *lua.LState) int {
		info := L.CheckTable(1)
		scriptID = luaStrField(info, "script_id")
		callback = luaStrField(info, "callback")
		return 0
	})

	fn := L.NewFunction(func(L *lua.LState) int {
		L.RaiseError("boom")
		return 0
	})
	callProtected(L, api, "timer_every", nil, fn)

	if scriptID != "test" {
		t.Errorf("script_id: want test, got %q", scriptID)
	}
	if callback != "timer_every" {
		t.Errorf("callback: want timer_every, got %q", callback)
	}
}

func TestLogFileException(t *testing.T) {
	L, api, _ := newHALState(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "errors.log")

	if err := L.DoString(`
		ha.on_exception(ha.exceptions.log_file("` + logPath + `"))
	`); err != nil {
		t.Fatal(err)
	}
	// Re-read the handler set by the Lua call
	api.onExceptionFn, _ = L.GetGlobal("ha").(*lua.LTable).RawGetString("on_exception").(*lua.LFunction)
	// Actually the handler was set into api via the Lua call - we need to reset api
	// Let's do it differently: call the exception directly.

	// Simulate calling the log_file handler
	haTbl := L.GetGlobal("ha").(*lua.LTable)
	excTbl := haTbl.RawGetString("exceptions").(*lua.LTable)
	logFileFn := excTbl.RawGetString("log_file").(*lua.LFunction)

	// Call log_file("path") → returns a handler
	if err := L.CallByParam(lua.P{Fn: logFileFn, NRet: 1, Protect: true},
		lua.LString(logPath)); err != nil {
		t.Fatal(err)
	}
	handler := L.Get(-1).(*lua.LFunction)
	L.Pop(1)

	// Build info table
	info := L.NewTable()
	info.RawSetString("script_id", lua.LString("lights"))
	info.RawSetString("error", lua.LString("boom"))
	info.RawSetString("traceback", lua.LString("stack: ..."))
	info.RawSetString("callback", lua.LString("state_changed"))
	info.RawSetString("timestamp", lua.LString("2026-01-01T00:00:00Z"))
	info.RawSetString("event", lua.LNil)

	if err := L.CallByParam(lua.P{Fn: handler, NRet: 0, Protect: true}, info); err != nil {
		t.Fatalf("log_file handler: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "boom") {
		t.Errorf("log file does not contain error: %s", data)
	}
	if !strings.Contains(string(data), "lights") {
		t.Errorf("log file does not contain script_id: %s", data)
	}
}

func TestGetState(t *testing.T) {
	L, _, tracker := newHALState(t)
	ctx := context.Background()

	_ = tracker.Seed(ctx, []ha.StateData{
		{EntityID: "light.test", State: "on", Attributes: jsontext.Value(`{"brightness":200}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	})

	if err := L.DoString(`_s = ha.get_state("light.test")`); err != nil {
		t.Fatal(err)
	}
	s := L.GetGlobal("_s").(*lua.LTable)
	if s.RawGetString("state") != lua.LString("on") {
		t.Errorf("state: want on, got %v", s.RawGetString("state"))
	}
}

func TestOnStateChangeRegistration(t *testing.T) {
	L, api, _ := newHALState(t)

	if err := L.DoString(`ha.on_state_change("light.*", function(data) end)`); err != nil {
		t.Fatal(err)
	}
	if len(api.stateChangeHandlers) != 1 {
		t.Errorf("expected 1 handler, got %d", len(api.stateChangeHandlers))
	}
	if api.stateChangeHandlers[0].pattern != "light.*" {
		t.Errorf("pattern: %q", api.stateChangeHandlers[0].pattern)
	}
}
