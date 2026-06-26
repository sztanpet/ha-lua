package lua

import (
	"context"
	"testing"
	"time"

	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
)

func stateChangedEvent(entity, stateVal string) ha.Event {
	return ha.Event{
		Type: "state_changed",
		Data: jsontext.Value(`{"entity_id":"` + entity + `","new_state":{"state":"` + stateVal + `","attributes":{}}}`),
	}
}

// TestRunnerCoalescesStateChanges: two changes to the same entity inside the
// batch window collapse to one dispatch carrying the newest state.
func TestRunnerCoalescesStateChanges(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "co.lua", `
		ha.on_state_change("sensor.x", function(data)
		  global.set("count", tostring((tonumber(global.get("count")) or 0) + 1))
		  global.set("last", data.new_state.state)
		end)
		global.set("loaded", "co")`)

	ctx, cancel := context.WithCancel(context.Background())
	sup, reg, global := newSupervisor(t, dir)
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); sup.Wait() }()
	globalString(t, global, "loaded", "co") // script registered + running

	reg.Dispatch(stateChangedEvent("sensor.x", "a"))
	reg.Dispatch(stateChangedEvent("sensor.x", "b")) // same entity, same window

	globalString(t, global, "last", "b") // the coalesced dispatch carries the latest
	time.Sleep(3 * batchWindow)          // let any (wrongly) un-coalesced second dispatch land
	if got, _ := global.Get(context.Background(), "count"); got != "1" {
		t.Errorf("handler ran %v times, want 1 (coalesced)", got)
	}
}

// TestRunnerImmediateEvents: ha.immediate_events() disables coalescing, so both
// changes are delivered.
func TestRunnerImmediateEvents(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "im.lua", `
		ha.immediate_events()
		ha.on_state_change("sensor.x", function(data)
		  global.set("count", tostring((tonumber(global.get("count")) or 0) + 1))
		end)
		global.set("loaded", "im")`)

	ctx, cancel := context.WithCancel(context.Background())
	sup, reg, global := newSupervisor(t, dir)
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); sup.Wait() }()
	globalString(t, global, "loaded", "im")

	reg.Dispatch(stateChangedEvent("sensor.x", "a"))
	reg.Dispatch(stateChangedEvent("sensor.x", "b"))

	globalString(t, global, "count", "2") // both transitions delivered
}
