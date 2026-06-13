package lua

import (
	"context"
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

	// Event loop: we expect "after" because "every" is 1h away.
	select {
	case ev := <-runner.ch:
		runner.handleEvent(L, api, ev)
	case <-time.After(time.Second):
		t.Fatal("timer did not fire")
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
