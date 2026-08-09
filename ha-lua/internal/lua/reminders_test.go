package lua

import (
	"context"
	"database/sql"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// newRemindersState boots an LState with the real ha/store/stdlib bindings and
// a require rooted in examples/lib, so the shipped lib/reminders.lua is what
// runs. Passing the same handles twice models a daemon restart: a new VM, the
// same SQLite store.
func newRemindersState(t *testing.T, writeDB, readDB *sql.DB) (*lua.LState, *sql.DB, *sql.DB) {
	t.Helper()
	if writeDB == nil {
		writeDB, readDB = testutil.NewTestDB(t, nil)
		if err := state.Migrate(writeDB); err != nil {
			t.Fatal(err)
		}
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	L.SetContext(context.Background())
	t.Cleanup(L.Close)
	RegisterStdlib(L, repoScriptsDir, openTestRoot(t, repoScriptsDir))

	sched := scheduler.New(writeDB, time.UTC, func(string, string) {})
	runner := &Runner{scriptID: "reminders", logsRoot: openTestRoot(t, t.TempDir())}
	api := &haAPI{scriptID: "reminders", scheduler: sched, timerFns: map[string]*lua.LFunction{}}
	runner.registerHaAPI(L, api)
	registerStoreAPI(L, store.New(writeDB, readDB, "reminders"), store.NewGlobal(writeDB, readDB))

	return L, writeDB, readDB
}

func TestRemindersFireAndCancel(t *testing.T) {
	L, _, _ := newRemindersState(t, nil, nil)

	// A negative delay is already due, so tick() is the clock here.
	err := L.DoString(`
		local reminders = require "reminders"
		fired = {}
		reminders.define("note", function(payload) fired[#fired+1] = payload.message end)
		reminders.start()

		reminders.schedule("door", "note", "10m", { message = "still open" })
		reminders.tick()
		assert(#fired == 0, "fired before it was due")
		assert(reminders.due_at("door") ~= nil, "not pending")

		reminders.schedule("door", "note", -1, { message = "still open" })
		reminders.tick()
		assert(#fired == 1, "want one fire, got "..#fired)
		assert(fired[1] == "still open", "payload lost: "..tostring(fired[1]))

		-- A fired reminder is gone, not re-fired on every later tick.
		reminders.tick()
		assert(#fired == 1, "fired again after firing")
		assert(reminders.due_at("door") == nil, "still pending after firing")

		-- Scheduling the same key twice replaces rather than stacks.
		reminders.schedule("door", "note", -1, { message = "first" })
		reminders.schedule("door", "note", -1, { message = "second" })
		reminders.tick()
		assert(#fired == 2, "replacement stacked: "..#fired)
		assert(fired[2] == "second", "wrong survivor: "..tostring(fired[2]))

		reminders.schedule("door", "note", -1, {})
		assert(reminders.cancel("door") == true, "cancel reported nothing to cancel")
		reminders.tick()
		assert(#fired == 2, "cancelled reminder fired")
		assert(reminders.cancel("nope") == false, "cancel invented a reminder")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

// The reason this library exists: a reminder armed inside a handler must still
// fire after a restart, which ha.after cannot promise.
func TestRemindersSurviveRestart(t *testing.T) {
	L, writeDB, readDB := newRemindersState(t, nil, nil)

	if err := L.DoString(`
		local reminders = require "reminders"
		reminders.define("note", function() end)
		reminders.start()
		reminders.schedule("door", "note", "10m", { message = "still open" })
	`); err != nil {
		t.Fatal(err)
	}

	// Restart: new VM, same store, and the reminder is now overdue.
	L2, _, _ := newRemindersState(t, writeDB, readDB)
	if err := L2.DoString(`
		local reminders = require "reminders"
		fired = 0
		reminders.define("note", function() fired = fired + 1 end)
		reminders.schedule("door", "note", -1, { message = "still open" })

		-- start() ticks immediately: what came due while we were down is
		-- exactly what the user is waiting on.
		reminders.start()
		assert(fired == 1, "overdue reminder did not survive the restart: "..fired)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestRemindersEscalate(t *testing.T) {
	L, _, _ := newRemindersState(t, nil, nil)

	err := L.DoString(`
		local reminders = require "reminders"
		steps, finals = {}, {}
		reminders.define("nag", function(payload)
			steps[#steps+1] = payload.step
			finals[#finals+1] = payload.final
		end)
		reminders.start()

		reminders.escalate("leak", "nag", { -1, -1, -1 }, {})
		reminders.tick()
		reminders.tick()
		reminders.tick()
		reminders.tick()

		assert(#steps == 3, "want 3 rungs, got "..#steps)
		for i = 1, 3 do assert(steps[i] == i, "rung "..i.." reported step "..tostring(steps[i])) end
		assert(finals[1] == false and finals[2] == false, "early rung marked final")
		assert(finals[3] == true, "last rung not marked final")

		-- A cancelled ladder stops where it is.
		reminders.escalate("leak2", "nag", { -1, -1 }, {})
		reminders.tick()
		reminders.cancel("leak2")
		reminders.tick()
		assert(#steps == 4, "cancelled ladder kept climbing: "..#steps)
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRemindersThrottle(t *testing.T) {
	L, writeDB, readDB := newRemindersState(t, nil, nil)

	if err := L.DoString(`
		local reminders = require "reminders"
		assert(reminders.throttle("battery", "6h") == true, "first call blocked")
		assert(reminders.throttle("battery", "6h") == false, "second call passed")
		assert(reminders.throttle("other", "6h") == true, "keys not independent")
		assert(reminders.forget("battery") == true, "forget found nothing")
		assert(reminders.throttle("battery", "6h") == true, "forget did not reopen the gate")
	`); err != nil {
		t.Fatal(err)
	}

	// A restart loop must not turn one notification per window into one per boot.
	L2, _, _ := newRemindersState(t, writeDB, readDB)
	if err := L2.DoString(`
		local reminders = require "reminders"
		assert(reminders.throttle("battery", "6h") == false, "throttle reset across restart")
	`); err != nil {
		t.Fatal(err)
	}
}

// An action nobody defined must not wedge the tick or re-fire forever.
func TestRemindersUnknownActionIsDropped(t *testing.T) {
	L, _, _ := newRemindersState(t, nil, nil)

	err := L.DoString(`
		local reminders = require "reminders"
		reminders.start()
		reminders.schedule("orphan", "never_defined", -1, {})
		reminders.tick()
		assert(reminders.due_at("orphan") == nil, "orphan stayed pending")
	`)
	if err != nil {
		t.Fatal(err)
	}
}
