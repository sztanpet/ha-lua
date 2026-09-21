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

// The entity ids the shipped example ships with. Unlike mirrored_switches this
// example carries placeholders, not the author's own entities — the user edits
// the copy in scripts/, never examples/ — so pinning them here is pinning the
// shipped file, which is the point.
var (
	groupSwitches = []string{
		"switch.hall_switch_a",
		"switch.hall_switch_b",
		"switch.hall_switch_c",
	}
	groupLights = []string{
		"light.hall_lamp_1",
		"light.hall_lamp_2",
		"light.hall_lamp_3",
	}
)

// groupHarness runs the real examples/group_switches.lua against a spy call
// service and the production tracker, in main.go's apply-then-dispatch order.
type groupHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	cmds    chan string // "turn_on light.a,light.b,light.c"
}

// newGroupHarness seeds every lamp to lampStates[i] and every switch to "off".
func newGroupHarness(t *testing.T, lampStates ...string) *groupHarness {
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
	for _, entityID := range groupSwitches {
		seed = append(seed, seedEntity(entityID, "off", `{}`))
	}
	for i, entityID := range groupLights {
		seed = append(seed, seedEntity(entityID, lampStates[i], `{}`))
	}
	if err := tracker.Seed(ctx, seed); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("group_switches", dir, openTestRoot(t, dir), nil,
		tracker, sched, store.New(writeDB, readDB, "group_switches"), global)
	r.SetCallServiceAsync(func(_ context.Context, _, service string, data jsontext.Value) (<-chan error, error) {
		var payload struct {
			EntityID any `json:"entity_id"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("bad service data %s: %v", data, err)
		}
		h.cmds <- service + " " + strings.Join(entityIDs(payload.EntityID), ",")
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
	want := service + " " + strings.Join(groupLights, ",")
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

// TestGroupSwitchesEverySwitchDrivesTheGroup: all three switches are equal, and
// a press always commands every lamp — both directions, from any of them.
func TestGroupSwitchesEverySwitchDrivesTheGroup(t *testing.T) {
	h := newGroupHarness(t, "off", "off", "off")

	for i, entityID := range groupSwitches {
		h.report(entityID, "off", "on")
		h.expectCmd("turn_on")
		for _, lamp := range groupLights { // the lamps confirm, as devices do
			h.report(lamp, "off", "on")
		}
		h.report(entityID, "on", "off")
		h.expectCmd("turn_off")
		for _, lamp := range groupLights {
			h.report(lamp, "on", "off")
		}
		if i == len(groupSwitches)-1 {
			h.expectSilence() // the lamps' own reports are never presses
		}
	}
}

// TestGroupSwitchesForcesHalfLitRoom: one lamp on is "the group is on", so the
// press takes everything off — and it commands ALL lamps, where a per-lamp
// toggle would leave the room half-lit with the other lamp on instead.
func TestGroupSwitchesForcesHalfLitRoom(t *testing.T) {
	h := newGroupHarness(t, "off", "on", "off")

	h.report(groupSwitches[1], "off", "on")
	h.expectCmd("turn_off")
}

// TestGroupSwitchesFastDoublePress reproduces the field bug the equivalent HA
// automation has: the lamps' reported state lags the command, so a second press
// inside the round trip reads "all off" from the mirror and turns everything on
// again, losing the press. The decision must come from our own last command
// while it is fresh.
func TestGroupSwitchesFastDoublePress(t *testing.T) {
	h := newGroupHarness(t, "off", "off", "off")

	h.report(groupSwitches[0], "off", "on")
	h.expectCmd("turn_on")
	// No lamp has reported anything yet — the mirror still says "off".
	h.report(groupSwitches[2], "off", "on")
	h.expectCmd("turn_off")
}

// TestGroupSwitchesIgnoresNonPresses: a relay leaving and rejoining the Zigbee
// mesh is not somebody flipping the switch, and neither is an attribute-only
// update. Either one flipping the room is the 3am bug.
func TestGroupSwitchesIgnoresNonPresses(t *testing.T) {
	h := newGroupHarness(t, "off", "off", "off")

	h.report(groupSwitches[0], "on", "unavailable")
	h.report(groupSwitches[0], "unavailable", "on")
	h.report(groupSwitches[1], "on", "on")
	h.expectSilence()
}
