package lua

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// TestCallServiceNoWait: with { wait = false } the handler returns before
// HA's verdict exists at all, and a late rejection is routed to the script's
// on_exception handler instead of raising in the (long finished) callback.
func TestCallServiceNoWait(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "async.lua", `
		ha.immediate_events()
		ha.on_exception(function(info)
		  global.set("exc", info.error)
		end)
		ha.on_state_change("sensor.x", function(data)
		  ha.call_service("switch", "turn_on", { entity_id = "switch.y" }, { wait = false })
		  global.set("after_call", "yes")
		end)
		global.set("loaded", "async")`)

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	// The verdict channel stays unresolved until the test decides — that is
	// the whole point: the handler must not be waiting on it.
	verdict := make(chan error, 1)
	sent := make(chan string, 1)
	r := NewRunner("async", dir, openTestRoot(t, dir), nil, tracker, sched,
		store.New(writeDB, readDB, "async"), global)
	r.SetCallServiceAsync(func(_ context.Context, domain, service string, _ jsontext.Value) (<-chan error, error) {
		sent <- domain + "." + service
		return verdict, nil
	})
	reg.Add(r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "async.lua")) }()
	t.Cleanup(func() { cancel(); <-done })
	globalString(t, global, "loaded", "async")

	reg.Dispatch(stateChangedEvent("sensor.x", "on"))

	// The command was sent and the handler completed — with the verdict
	// still pending. Under wait=true this would deadlock right here.
	select {
	case got := <-sent:
		if got != "switch.turn_on" {
			t.Fatalf("sent %q, want switch.turn_on", got)
		}
	case <-time.After(3 * time.Second):
		exc, _ := global.Get(context.Background(), "exc")
		t.Fatalf("async send never happened (exc=%v)", exc)
	}
	globalString(t, global, "after_call", "yes")

	// A late rejection lands in on_exception with the call spelled out.
	verdict <- errors.New("device exploded")
	globalString(t, global, "exc",
		"call_service switch.turn_on (wait=false): device exploded")
}
