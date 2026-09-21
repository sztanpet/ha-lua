package lua

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// The entity ids hardcoded in the shipped example. Two things here are the
// whole point of the script and must stay pinned: the lamps mix domains (a
// light and a relay, so a light.* call would silently skip the relay), and the
// last switch IS one of the lamps (so it both triggers and echoes).
var (
	groupSwitches = []string{
		"switch.halo_ajtokapcsolo",
		"switch.halo_ajtoszekrenykapcsolo",
		"switch.galeria_lepcsokapcsolo",
	}
	groupLamps = []string{
		"light.bedroom_galeria_halo_led",
		"switch.galeria_lepcsokapcsolo",
	}
	// The relay that is both a trigger and a lamp.
	groupRelay = "switch.galeria_lepcsokapcsolo"
)

// groupHarness runs the real examples/group_switches.lua against a spy call
// service and the production tracker, in main.go's apply-then-dispatch order.
type groupHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	cmds    chan string // "homeassistant.turn_on light.a,switch.b"
}

// newGroupHarness seeds every lamp to lampStates[i] and every pure-input switch
// to "off".
func newGroupHarness(t *testing.T, lampStates ...string) *groupHarness {
	if len(lampStates) != len(groupLamps) {
		t.Fatalf("seed %d lamp states, the example has %d lamps", len(lampStates), len(groupLamps))
	}
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "group_switches.lua"),
		filepath.Join(dir, "group_switches.lua"))

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
	h := &groupHarness{t: t, ctx: ctx, tracker: tracker, reg: reg,
		cmds: make(chan string, 16)}

	var seed []ha.StateData
	for i, entityID := range groupLamps {
		seed = append(seed, seedEntity(entityID, lampStates[i], `{}`))
	}
	for _, entityID := range groupSwitches {
		if !isGroupLamp(entityID) {
			seed = append(seed, seedEntity(entityID, "off", `{}`))
		}
	}
	if err := tracker.Seed(ctx, seed); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("group_switches", dir, openTestRoot(t, dir), nil,
		tracker, sched, store.New(writeDB, readDB, "group_switches"), global)
	r.SetCallServiceAsync(func(_ context.Context, domain, service string, data jsontext.Value) (<-chan error, error) {
		var payload struct {
			EntityID any `json:"entity_id"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("bad service data %s: %v", data, err)
		}
		h.cmds <- domain + "." + service + " " + strings.Join(entityIDs(payload.EntityID), ",")
		verdict := make(chan error, 1)
		verdict <- nil
		return verdict, nil
	})
	reg.Add(r)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "group_switches.lua")) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("group_switches.lua did not finish loading")
	}
	return h
}

func isGroupLamp(entityID string) bool {
	for _, lamp := range groupLamps {
		if lamp == entityID {
			return true
		}
	}
	return false
}

// report feeds one state_changed carrying both states, the way Home Assistant
// sends it — deciding which of those are presses is the script's job.
func (h *groupHarness) report(entityID, oldState, newState string) {
	h.t.Helper()
	entity := func(stateVal string) string {
		return `{"entity_id":"` + entityID + `","state":"` + stateVal +
			`","attributes":{},"last_changed":"2026-01-01T01:00:00Z",` +
			`"last_updated":"2026-01-01T01:00:00Z"}`
	}
	raw := jsontext.Value(`{"entity_id":"` + entityID + `","old_state":` +
		entity(oldState) + `,"new_state":` + entity(newState) + `}`)
	if err := h.tracker.HandleStateChanged(h.ctx, raw); err != nil {
		h.t.Fatal(err)
	}
	h.reg.Dispatch(ha.Event{Type: "state_changed", Data: raw})
}

func (h *groupHarness) expectCmd(service string) {
	h.t.Helper()
	want := "homeassistant." + service + " " + strings.Join(groupLamps, ",")
	select {
	case got := <-h.cmds:
		if got != want {
			h.t.Fatalf("service call = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		h.t.Fatalf("no service call, want %q", want)
	}
}

func (h *groupHarness) expectSilence() {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		h.t.Fatalf("unexpected service call %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestGroupSwitchesPureInputTogglesGroup: a switch that is not itself a lamp is
// a pure input, so either direction of flip toggles the group — and every lamp
// is commanded, in one homeassistant.* call that spans both domains.
func TestGroupSwitchesPureInputTogglesGroup(t *testing.T) {
	for _, entityID := range groupSwitches {
		if isGroupLamp(entityID) {
			continue
		}
		t.Run(entityID, func(t *testing.T) {
			h := newGroupHarness(t, "off", "off")

			h.report(entityID, "off", "on")
			h.expectCmd("turn_on")
			for _, lamp := range groupLamps { // the lamps confirm, as devices do
				h.report(lamp, "off", "on")
			}
			h.expectSilence() // including the relay's echo of our own command

			h.report(entityID, "on", "off") // a flip, not a copy of the state
			h.expectCmd("turn_off")
		})
	}
}

// TestGroupSwitchesRelayPressFollowsIt: the relay's own wall switch has already
// changed the lamp it feeds, so its new state is the intent and the rest are
// forced to match. Toggling the group here would take the light the person just
// switched on straight back off.
func TestGroupSwitchesRelayPressFollowsIt(t *testing.T) {
	h := newGroupHarness(t, "off", "off")

	h.report(groupRelay, "off", "on")
	h.expectCmd("turn_on")

	h.report(groupRelay, "on", "off")
	h.expectCmd("turn_off")
}

// TestGroupSwitchesSwallowsOwnEcho is the strobe regression: the relay is both
// commanded and watched, so our command comes back as a state report. Acted on
// as a press it would restart the decision, and with our command still fresh
// the verdict flips — the room oscillates until the echo deadline.
func TestGroupSwitchesSwallowsOwnEcho(t *testing.T) {
	h := newGroupHarness(t, "off", "off")

	h.report(groupSwitches[0], "off", "on")
	h.expectCmd("turn_on")
	h.report(groupRelay, "off", "on") // our own command reporting back
	h.expectSilence()

	// The echo is consumed once: the next report on the same entity is a real
	// press again, and it sets the direction.
	h.report(groupRelay, "on", "off")
	h.expectCmd("turn_off")
}

// TestGroupSwitchesForcesHalfLitRoom: one lamp on is "the group is on", so the
// press takes everything off — and it commands ALL lamps, where a per-lamp
// toggle would leave the room half-lit with the other lamp on instead.
func TestGroupSwitchesForcesHalfLitRoom(t *testing.T) {
	h := newGroupHarness(t, "off", "on")

	h.report(groupSwitches[0], "off", "on")
	h.expectCmd("turn_off")
}

// TestGroupSwitchesFastDoublePress reproduces the field bug the equivalent HA
// automation has: the lamps' reported state lags the command, so a second press
// inside the round trip reads "all off" from the mirror and turns everything on
// again, losing the press. The decision must come from our own last command
// while it is fresh.
func TestGroupSwitchesFastDoublePress(t *testing.T) {
	h := newGroupHarness(t, "off", "off")

	h.report(groupSwitches[0], "off", "on")
	h.expectCmd("turn_on")
	// No lamp has reported anything yet — the mirror still says "off".
	h.report(groupSwitches[0], "on", "off")
	h.expectCmd("turn_off")
}

// TestGroupSwitchesIgnoresNonPresses: a relay leaving and rejoining the Zigbee
// mesh is not somebody flipping the switch, and neither is an attribute-only
// update. Either one flipping the room is the 3am bug.
func TestGroupSwitchesIgnoresNonPresses(t *testing.T) {
	h := newGroupHarness(t, "off", "off")

	for _, entityID := range []string{groupSwitches[0], groupRelay} {
		h.report(entityID, "on", "unavailable")
		h.report(entityID, "unavailable", "on")
		h.report(entityID, "on", "on")
	}
	h.expectSilence()
}
