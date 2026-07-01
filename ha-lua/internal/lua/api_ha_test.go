package lua

import (
	"context"
	"fmt"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
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

func newHALState(t testing.TB) (*lua.LState, *haAPI, *state.Tracker, *Runner) {
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

	var runner *Runner
	sched := scheduler.New(writeDB, time.UTC, func(scriptID, timerID string) {
		if runner != nil {
			runner.Send(Event{TimerFired: &TimerFiredEvent{TimerID: timerID}})
		}
	})

	runner = &Runner{
		scriptID:  "test",
		scriptDir: t.TempDir(),
		ch:        make(chan Event, 8),
		timerFns:  make(map[string]*lua.LFunction),
		LoadedCh:  make(chan struct{}),
		tracker:   tracker,
		scheduler: sched,
		kv:        kv,
		global:    global,
	}
	api := &haAPI{scriptID: "test", tracker: tracker, scheduler: sched, timerFns: runner.timerFns}
	runner.registerHaAPI(L, api)
	registerStoreAPI(L, kv, global)
	return L, api, tracker, runner
}

func TestOnExceptionCalled(t *testing.T) {
	L, api, _, _ := newHALState(t)

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
	L, api, _, _ := newHALState(t)

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
	L, api, _, _ := newHALState(t)
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

func TestEmailExceptionCooldown(t *testing.T) {
	L, _, _, _ := newHALState(t)

	var sent []string
	orig := smtpSendMail
	smtpSendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		sent = append(sent, string(msg))
		return nil
	}
	t.Cleanup(func() { smtpSendMail = orig })

	haTbl := L.GetGlobal("ha").(*lua.LTable)
	excTbl := haTbl.RawGetString("exceptions").(*lua.LTable)
	emailFn := excTbl.RawGetString("email").(*lua.LFunction)

	cfg := L.NewTable()
	cfg.RawSetString("to", lua.LString("user@example.com"))
	cfg.RawSetString("smtp_host", lua.LString("localhost"))
	cfg.RawSetString("smtp_port", lua.LNumber(25))
	cfg.RawSetString("username", lua.LString("u"))
	cfg.RawSetString("password", lua.LString("p"))
	cfg.RawSetString("cooldown", lua.LString("100ms"))

	if err := L.CallByParam(lua.P{Fn: emailFn, NRet: 1, Protect: true}, cfg); err != nil {
		t.Fatal(err)
	}
	handler := L.Get(-1).(*lua.LFunction)
	L.Pop(1)

	fire := func(ts string) {
		info := L.NewTable()
		info.RawSetString("script_id", lua.LString("test"))
		info.RawSetString("error", lua.LString("boom"))
		info.RawSetString("callback", lua.LString("state_changed"))
		info.RawSetString("timestamp", lua.LString(ts))
		if err := L.CallByParam(lua.P{Fn: handler, NRet: 0, Protect: true}, info); err != nil {
			t.Fatalf("email handler: %v", err)
		}
	}

	// Three errors in quick succession: only the first is sent.
	fire("2026-01-01T00:00:00Z")
	fire("2026-01-01T00:00:01Z")
	fire("2026-01-01T00:00:02Z")
	if len(sent) != 1 {
		t.Fatalf("sends inside cooldown: want 1, got %d", len(sent))
	}

	// After the window, the next error is sent and reports the suppressed two.
	time.Sleep(150 * time.Millisecond)
	fire("2026-01-01T00:05:00Z")
	if len(sent) != 2 {
		t.Fatalf("sends after cooldown: want 2, got %d", len(sent))
	}
	if !strings.Contains(sent[1], "2 similar errors suppressed since 2026-01-01T00:00:01Z") {
		t.Errorf("second email missing suppression note:\n%s", sent[1])
	}
	if strings.Contains(sent[0], "suppressed") {
		t.Errorf("first email must not have a suppression note:\n%s", sent[0])
	}
}

func TestGetState(t *testing.T) {
	L, _, tracker, _ := newHALState(t)
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
	L, api, _, _ := newHALState(t)

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

// The traceback must be the Lua stack trace, not a second copy of the
// error message.
func TestExceptionTraceback(t *testing.T) {
	L, api, _, _ := newHALState(t)

	var errMsg, traceback string
	api.onExceptionFn = L.NewFunction(func(L *lua.LState) int {
		info := L.CheckTable(1)
		errMsg = luaStrField(info, "error")
		traceback = luaStrField(info, "traceback")
		return 0
	})

	if err := L.DoString(`function boom() error("kaboom") end`); err != nil {
		t.Fatal(err)
	}
	fn, _ := L.GetGlobal("boom").(*lua.LFunction)
	callProtected(L, api, "state_changed", lua.LNil, fn)

	if !strings.Contains(errMsg, "kaboom") {
		t.Errorf("error: %q", errMsg)
	}
	if !strings.Contains(traceback, "traceback") {
		t.Errorf("traceback missing stack trace: %q", traceback)
	}
	if strings.Contains(errMsg, "traceback") {
		t.Errorf("error message contains the stack trace: %q", errMsg)
	}
}

func TestOnStateChangeBadPattern(t *testing.T) {
	L, _, _, _ := newHALState(t)

	err := L.DoString(`ha.on_state_change("light.[", function() end)`)
	if err == nil {
		t.Fatal("expected load-time error for malformed pattern")
	}
	if !strings.Contains(err.Error(), "bad pattern") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestScriptID(t *testing.T) {
	L, _, _, _ := newHALState(t)
	if err := L.DoString(`sid = ha.script_id`); err != nil {
		t.Fatal(err)
	}
	if got := L.GetGlobal("sid").String(); got != "test" {
		t.Errorf("script_id: want test, got %q", got)
	}
}

// ha.set_state coerces a numeric state to a string, marshals the attrs table,
// and returns the created flag — the value/err pair, not a raise.
func TestSetStateBinding(t *testing.T) {
	L, api, _, _ := newHALState(t)

	var gotID, gotState, gotAttrs string
	api.setState = func(ctx context.Context, entityID, state string, attrs jsontext.Value) (bool, error) {
		gotID, gotState, gotAttrs = entityID, state, string(attrs)
		return true, nil
	}

	if err := L.DoString(`created, err = ha.set_state("sensor.x", 21.5, { unit_of_measurement = "°C" })`); err != nil {
		t.Fatal(err)
	}
	if gotID != "sensor.x" || gotState != "21.5" {
		t.Errorf("got id=%q state=%q", gotID, gotState)
	}
	if !strings.Contains(gotAttrs, "°C") {
		t.Errorf("attrs not marshalled: %q", gotAttrs)
	}
	if L.GetGlobal("created") != lua.LTrue {
		t.Errorf("created: want true, got %v", L.GetGlobal("created"))
	}
	if L.GetGlobal("err") != lua.LNil {
		t.Errorf("err: want nil, got %v", L.GetGlobal("err"))
	}
}

// An operational set_state failure must return nil, errmsg — never raise — so
// the per-minute publish loop doesn't spam on_exception during an outage.
func TestSetStateBindingNonRaising(t *testing.T) {
	L, api, _, _ := newHALState(t)
	api.setState = func(ctx context.Context, entityID, state string, attrs jsontext.Value) (bool, error) {
		return false, fmt.Errorf("network down")
	}

	if err := L.DoString(`v, err = ha.set_state("sensor.x", "on")`); err != nil {
		t.Fatalf("set_state must not raise on an operational error: %v", err)
	}
	if L.GetGlobal("v") != lua.LNil {
		t.Errorf("value: want nil, got %v", L.GetGlobal("v"))
	}
	if s, ok := L.GetGlobal("err").(lua.LString); !ok || !strings.Contains(string(s), "network down") {
		t.Errorf("err: want 'network down', got %v", L.GetGlobal("err"))
	}
}

func TestRemoveStateBinding(t *testing.T) {
	L, api, _, _ := newHALState(t)
	var gotID string
	api.removeState = func(ctx context.Context, entityID string) error {
		gotID = entityID
		return nil
	}
	if err := L.DoString(`ok, err = ha.remove_state("sensor.x")`); err != nil {
		t.Fatal(err)
	}
	if gotID != "sensor.x" {
		t.Errorf("entity id: got %q", gotID)
	}
	if L.GetGlobal("ok") != lua.LTrue || L.GetGlobal("err") != lua.LNil {
		t.Errorf("want true,nil; got %v,%v", L.GetGlobal("ok"), L.GetGlobal("err"))
	}
}

// ha.on_command registers a single ha_lua_command handler and routes only
// commands addressed to this script, calling handler(action, data).
func TestOnCommand(t *testing.T) {
	L, api, _, runner := newHALState(t)
	if err := L.DoString(`
		count = 0
		last_action = nil
		last_entity = nil
		ha.on_command(function(action, data)
			count = count + 1
			last_action = action
			last_entity = data.climate_entity
		end)
	`); err != nil {
		t.Fatal(err)
	}
	if len(api.eventHandlers) != 1 || api.eventHandlers[0].eventType != "ha_lua_command" {
		t.Fatalf("on_command did not register an ha_lua_command handler: %+v", api.eventHandlers)
	}

	fire := func(script, action string) {
		runner.handleHAEvent(L, api, ha.Event{
			Type: "ha_lua_command",
			Data: jsontext.Value(`{"script":"` + script + `","action":"` + action +
				`","data":{"climate_entity":"climate.lr"}}`),
		})
	}
	fire("test", "override") // ours — handled
	fire("other", "ignored") // different script — dropped

	if n := int(L.GetGlobal("count").(lua.LNumber)); n != 1 {
		t.Errorf("handler call count: want 1, got %d", n)
	}
	if got := L.GetGlobal("last_action").String(); got != "override" {
		t.Errorf("action: want override, got %q", got)
	}
	if got := L.GetGlobal("last_entity").String(); got != "climate.lr" {
		t.Errorf("entity: want climate.lr, got %q", got)
	}
}

func TestTimerAPI(t *testing.T) {
	L, api, _, runner := newHALState(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := api.scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}

	fired := make(chan string, 8)
	L.SetGlobal("fired", L.NewFunction(func(L *lua.LState) int {
		fired <- L.CheckString(1)
		return 0
	}))

	// 1. ha.every - use a long interval so it only fires once for catch-up
	// if we were to test catch-up, but here it's a fresh registration.
	// Actually, the loop fires it because nextRun is set to now+interval.
	// Wait, RegisterEvery sets nextRun to now+d.
	if err := L.DoString(`
		ha.every("1h", function() fired("every") end)
	`); err != nil {
		t.Fatal(err)
	}

	// 2. ha.after - use a short interval to fire soon
	if err := L.DoString(`
		ha.after("10ms", function() fired("after") end)
	`); err != nil {
		t.Fatal(err)
	}

	var afterKey string
	for k := range runner.timerFns {
		if strings.Contains(k, "|after|") {
			afterKey = k
			break
		}
	}
	if afterKey == "" {
		t.Fatal("after timer not found in runner.timerFns")
	}

	// Event loop: we expect "after" because "every" is 1h away.
	select {
	case ev := <-runner.ch:
		runner.handleEvent(L, api, ev)
	case <-time.After(time.Second):
		t.Fatal("timer did not fire")
	}

	if _, ok := runner.timerFns[afterKey]; ok {
		t.Errorf("after timer %q not deleted from runner.timerFns after firing", afterKey)
	}

	select {
	case tag := <-fired:
		if tag != "after" {
			t.Errorf("got %q, want after", tag)
		}
	default:
		t.Fatal("fired channel empty")
	}

	// 3. ha.at
	if err := L.DoString(`
		ha.at("07:00", function() end)
	`); err != nil {
		t.Fatal(err)
	}
}

// TestPruneKeepsLoadTimeAfter: the runner prunes with api.keepIDs after load,
// and that set must include load-time ha.after registrations — pruning their
// rows silently lost the restart-orphan warning. The ha.after between the two
// ha.every calls must also not shift the every IDs: the seq in the stable ID
// is what carries last_run/next_run across reloads.
func TestPruneKeepsLoadTimeAfter(t *testing.T) {
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	L := lua.NewState()
	defer L.Close()
	L.SetContext(ctx)

	sched := scheduler.New(writeDB, time.UTC, func(string, string) {})
	runner := &Runner{scriptID: "test", timerFns: make(map[string]*lua.LFunction), scheduler: sched}
	api := &haAPI{scriptID: "test", scheduler: sched, timerFns: runner.timerFns}
	runner.registerHaAPI(L, api)

	if err := L.DoString(`
		ha.every("1h", function() end)
		ha.after("1h", function() end)
		ha.every("2h", function() end)
	`); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"test|every|1h|1", "test|every|2h|2"} {
		found := false
		for _, id := range api.keepIDs {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("stable ID %q missing from keepIDs %v", want, api.keepIDs)
		}
	}

	if err := sched.PruneScript(ctx, "test", api.keepIDs); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM timers WHERE script_id = 'test'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("timer rows after load-time prune: want 3, got %d", n)
	}
}

func TestTimerExceptionHandling(t *testing.T) {
	L, api, _, runner := newHALState(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := api.scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}

	var caughtErr, caughtCallback, caughtTraceback string
	api.onExceptionFn = L.NewFunction(func(L *lua.LState) int {
		info := L.CheckTable(1)
		caughtErr = luaStrField(info, "error")
		caughtCallback = luaStrField(info, "callback")
		caughtTraceback = luaStrField(info, "traceback")
		return 0
	})

	// Register a failing after timer
	if err := L.DoString(`
		ha.after("10ms", function()
			error("timer fail")
		end)
	`); err != nil {
		t.Fatal(err)
	}

	// Wait for the timer to fire and deliver to the runner's channel
	select {
	case ev := <-runner.ch:
		runner.handleEvent(L, api, ev)
	case <-time.After(time.Second):
		t.Fatal("timer did not fire")
	}

	if !strings.Contains(caughtErr, "timer fail") {
		t.Errorf("expected error 'timer fail', got %q", caughtErr)
	}
	if caughtCallback != "timer_after" {
		t.Errorf("expected callback 'timer_after', got %q", caughtCallback)
	}
	if !strings.Contains(caughtTraceback, "traceback") {
		t.Errorf("expected stack traceback, got %q", caughtTraceback)
	}
}
