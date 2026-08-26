package lua

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/mqtt"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// The topic, ids and step size hardcoded in the shipped example.
const (
	dimmerActionTopic  = "zigbee2mqtt/ikea dimmer 1/action"
	dimmerLight        = "light.konyha_konyha_led"
	dimmerMin          = 3
	dimmerOnBrightness = 255
)

// The example's ramp is geometric: each step multiplies the level by
// (255/3)^(0.15/4) ≈ 1.1813, so the steps below are the levels a hold from
// the seeded brightness actually walks through. A linear step is what made
// the ramp visibly staircase at the dim end.

// dimmerHarness runs the real examples/ikea_dimmer.lua against a spy call
// service, the production tracker and a live scheduler — the ramp is a chain
// of ha.after timers, so it only works with the scheduler actually running.
type dimmerHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	broker  *fakeBroker
	cmds    chan string // "turn_on 116" / "turn_on -" / "turn_off -"
}

func newDimmerHarness(t *testing.T, lightState string, lightAttrs string) *dimmerHarness {
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "ikea_dimmer.lua"),
		filepath.Join(dir, "ikea_dimmer.lua"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	tracker.Start(t.Context())
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	ctx, cancel := context.WithCancel(context.Background())
	h := &dimmerHarness{t: t, ctx: ctx, tracker: tracker, reg: reg,
		cmds: make(chan string, 32)}

	if err := tracker.Seed(ctx, []ha.StateData{seedEntity(dimmerLight, lightState, lightAttrs)}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("ikea_dimmer", dir, openTestRoot(t, dir), nil,
		tracker, sched, store.New(writeDB, readDB, "ikea_dimmer"), global)
	r.SetCallServiceAsync(func(_ context.Context, _, service string, data jsontext.Value) (<-chan error, error) {
		var payload struct {
			Brightness *int `json:"brightness"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("bad service data %s: %v", data, err)
		}
		level := "-"
		if payload.Brightness != nil {
			level = fmt.Sprint(*payload.Brightness)
		}
		h.cmds <- service + " " + level
		verdict := make(chan error, 1)
		verdict <- nil
		return verdict, nil
	})
	h.broker = &fakeBroker{}
	r.SetMQTT(h.broker.subscribe, h.broker.publish)
	reg.Add(r)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "ikea_dimmer.lua")) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("ikea_dimmer.lua did not finish loading")
	}
	return h
}

func seedEntity(entityID, stateVal, attrs string) ha.StateData {
	return ha.StateData{EntityID: entityID, State: stateVal,
		Attributes:  jsontext.Value(attrs),
		LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"}
}

// press feeds one press the way Zigbee2MQTT publishes it: a bare action word
// on the device's action topic.
func (h *dimmerHarness) press(action string) {
	h.t.Helper()
	h.reg.DispatchMQTT(mqtt.Message{Topic: dimmerActionTopic, Payload: []byte(action)})
}

func (h *dimmerHarness) expectCmd(want string) {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		if got != want {
			h.t.Fatalf("service call = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		h.t.Fatalf("no service call, want %q", want)
	}
}

// expectSilence outlasts one ramp step, so a step that should have been
// cancelled has time to fire and fail the test.
// expectUntil drains commands until want arrives, so a test can assert where
// a ramp ends up without pinning every level on the way.
func (h *dimmerHarness) expectUntil(want string) {
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-h.cmds:
			if got == want {
				return
			}
		case <-deadline:
			h.t.Fatalf("ramp never reached %q", want)
		}
	}
}

func (h *dimmerHarness) expectSilence() {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		h.t.Fatalf("unexpected service call %q", got)
	case <-time.After(600 * time.Millisecond):
	}
}

// TestIkeaDimmerClicks checks the two buttons map to on at ON_BRIGHTNESS and
// off.
func TestIkeaDimmerClicks(t *testing.T) {
	h := newDimmerHarness(t, "off", `{}`)

	h.press("on")
	h.expectCmd(fmt.Sprintf("turn_on %d", dimmerOnBrightness))
	h.press("off")
	h.expectCmd("turn_off -")
}

// TestIkeaDimmerHoldRamps is the whole point of the script: the device sends
// only "move" and "stop", so the daemon has to step the level itself until
// the release arrives — and stop stepping when it does.
func TestIkeaDimmerHoldRamps(t *testing.T) {
	h := newDimmerHarness(t, "on", `{"brightness":100}`)

	h.press("brightness_move_up")
	h.expectCmd("turn_on 118")
	h.expectCmd("turn_on 139")

	h.press("brightness_stop")
	h.expectSilence()
}

// TestIkeaDimmerHoldDownStopsAtMinimum checks a hold down dims to the bottom
// of the range and stays there: the light must never switch itself off, and a
// pinned ramp must stop resending the same level.
func TestIkeaDimmerHoldDownStopsAtMinimum(t *testing.T) {
	h := newDimmerHarness(t, "on", `{"brightness":20}`)

	h.press("brightness_move_down")
	h.expectCmd("turn_on 17")
	h.expectCmd("turn_on 14")
	// Down to the floor: near the bottom a multiplicative step rounds back to
	// where it started, so the ramp steps by 1 rather than stalling.
	h.expectUntil(fmt.Sprintf("turn_on %d", dimmerMin))
	h.expectSilence()
}

// TestIkeaDimmerHoldUpFromOff: holding up on a dark light lights it at the
// bottom and ramps from there. Holding down does nothing at all.
func TestIkeaDimmerHoldUpFromOff(t *testing.T) {
	h := newDimmerHarness(t, "off", `{}`)

	h.press("brightness_move_down")
	h.expectSilence()

	h.press("brightness_move_up")
	h.expectCmd(fmt.Sprintf("turn_on %d", dimmerMin))
	h.expectCmd("turn_on 4")
}

// TestIkeaDimmerIgnoresUnknownActions guards the dispatch table: a STYRBAR-ish
// extra action (or Zigbee2MQTT clearing the sensor back to "") must not touch
// the light.
func TestIkeaDimmerIgnoresUnknownActions(t *testing.T) {
	h := newDimmerHarness(t, "on", `{"brightness":100}`)

	h.press("arrow_left_click")
	h.press("")
	h.expectSilence()
}

// TestIkeaDimmerSubscribesToTheActionTopic pins the topic the script asks the
// broker for. The friendly name carries spaces verbatim — assuming the
// underscored entity-id spelling is exactly the mistake that made an earlier
// version of this script watch a topic that never existed.
func TestIkeaDimmerSubscribesToTheActionTopic(t *testing.T) {
	h := newDimmerHarness(t, "on", `{"brightness":100}`)

	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()
	if len(h.broker.filters) != 1 || h.broker.filters[0] != dimmerActionTopic {
		t.Errorf("subscribed filters = %q, want [%q]", h.broker.filters, dimmerActionTopic)
	}
}
