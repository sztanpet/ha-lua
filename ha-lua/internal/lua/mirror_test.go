package lua

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// The entity ids hardcoded in the shipped example.
const (
	mirrorSwitchA = "switch.zbminir2_bejaratiajtokapcsolo"
	mirrorSwitchB = "switch.zbminir2_folyoso"
)

// mirrorHarness runs the real examples/mirrored_switches.lua against a spy
// call service and the production tracker, replicating main.go's
// apply-then-dispatch router ordering.
type mirrorHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	cmds    chan string // "turn_on switch.x"
}

func newMirrorHarness(t *testing.T) *mirrorHarness {
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "mirrored_switches.lua"),
		filepath.Join(dir, "mirrored_switches.lua"))

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
	h := &mirrorHarness{t: t, ctx: ctx, tracker: tracker, reg: reg,
		cmds: make(chan string, 16)}

	if err := tracker.Seed(ctx, []ha.StateData{
		seedSwitch(mirrorSwitchA, "off"),
		seedSwitch(mirrorSwitchB, "off"),
	}); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("mirrored_switches", dir, openTestRoot(t, dir), nil,
		tracker, sched, store.New(writeDB, readDB, "mirrored_switches"), global)
	r.SetCallServiceAsync(func(_ context.Context, _, service string, data jsontext.Value) (<-chan error, error) {
		var payload struct {
			EntityID string `json:"entity_id"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("bad service data %s: %v", data, err)
		}
		h.cmds <- service + " " + payload.EntityID
		verdict := make(chan error, 1)
		verdict <- nil
		return verdict, nil
	})
	reg.Add(r)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "mirrored_switches.lua")) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("mirrored_switches.lua did not finish loading")
	}
	return h
}

func seedSwitch(entityID, stateVal string) ha.StateData {
	return ha.StateData{EntityID: entityID, State: stateVal,
		Attributes:  jsontext.Value(`{}`),
		LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"}
}

// report feeds one state_changed through the tracker and the registry, in
// main.go's router order (apply to the mirror, then dispatch).
func (h *mirrorHarness) report(entityID, stateVal string) {
	h.t.Helper()
	raw := jsontext.Value(`{"entity_id":"` + entityID + `","new_state":{"entity_id":"` +
		entityID + `","state":"` + stateVal + `","attributes":{},` +
		`"last_changed":"2026-01-01T01:00:00Z","last_updated":"2026-01-01T01:00:00Z"}}`)
	if err := h.tracker.HandleStateChanged(h.ctx, raw); err != nil {
		h.t.Fatal(err)
	}
	h.reg.Dispatch(ha.Event{Type: "state_changed", Data: raw})
}

func (h *mirrorHarness) expectCmd(want string) {
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

func (h *mirrorHarness) expectSilence() {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		h.t.Fatalf("unexpected service call %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestMirroredSwitchesFastToggle reproduces the field bug: toggling a switch
// faster than the partner's device round trip lost commands, because the old
// "partner already matches" guard compared against the partner's reported
// state, which still showed the pre-command value. The echo-attribution
// version must forward both flips and swallow both late echoes without
// bouncing them back.
func TestMirroredSwitchesFastToggle(t *testing.T) {
	h := newMirrorHarness(t)

	// Two flips of A, faster than B can physically follow.
	h.report(mirrorSwitchA, "on")
	h.expectCmd("turn_on " + mirrorSwitchB)
	h.report(mirrorSwitchA, "off") // B has not reported anything yet
	h.expectCmd("turn_off " + mirrorSwitchB)

	// B's reports for both commands arrive late; they are our echoes and
	// must not trigger anything (the old guard bounced A back on here).
	h.report(mirrorSwitchB, "on")
	h.report(mirrorSwitchB, "off")
	h.expectSilence()
}

// TestMirroredSwitchesPhysicalPress: a report with no outstanding command is
// a real press and must propagate — and its echo must then be consumed.
func TestMirroredSwitchesPhysicalPress(t *testing.T) {
	h := newMirrorHarness(t)

	h.report(mirrorSwitchB, "on")
	h.expectCmd("turn_on " + mirrorSwitchA)
	h.report(mirrorSwitchA, "on") // the echo of that command
	h.expectSilence()

	// Non-actionable states never propagate.
	h.report(mirrorSwitchA, "unavailable")
	h.expectSilence()
}
