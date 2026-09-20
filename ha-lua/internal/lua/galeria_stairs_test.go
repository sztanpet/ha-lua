package lua

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// The entity ids hardcoded in the shipped example.
const (
	galeriaSource = "switch.halo_ajtokapcsolo"
	galeriaTarget = "switch.galeria_lepcsokapcsolo"
)

// galeriaHarness runs the real examples/galeria_stairs.lua against a spy call
// service and the production tracker, in main.go's apply-then-dispatch order.
type galeriaHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	cmds    chan string // "toggle switch.x"
}

func newGaleriaHarness(t *testing.T) *galeriaHarness {
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "galeria_stairs.lua"),
		filepath.Join(dir, "galeria_stairs.lua"))

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
	h := &galeriaHarness{t: t, ctx: ctx, tracker: tracker, reg: reg,
		cmds: make(chan string, 16)}

	if err := tracker.Seed(ctx, []ha.StateData{
		seedEntity(galeriaSource, "off", `{}`),
		seedEntity(galeriaTarget, "off", `{}`),
	}); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("galeria_stairs", dir, openTestRoot(t, dir), nil,
		tracker, sched, store.New(writeDB, readDB, "galeria_stairs"), global)
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
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "galeria_stairs.lua")) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("galeria_stairs.lua did not finish loading")
	}
	return h
}

// report feeds one state_changed carrying both states, the way Home Assistant
// sends it — the script's whole job is deciding which of those are presses.
func (h *galeriaHarness) report(entityID, oldState, newState string) {
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

func (h *galeriaHarness) expectCmd(want string) {
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

func (h *galeriaHarness) expectSilence() {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		h.t.Fatalf("unexpected service call %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestGaleriaStairsTogglesBothDirections: the hall switch is a pulse, not a
// mirror — off->on and on->off both toggle the staircase relay.
func TestGaleriaStairsTogglesBothDirections(t *testing.T) {
	h := newGaleriaHarness(t)

	h.report(galeriaSource, "off", "on")
	h.expectCmd("toggle " + galeriaTarget)
	h.report(galeriaSource, "on", "off")
	h.expectCmd("toggle " + galeriaTarget)
}

// TestGaleriaStairsIgnoresNonPresses: a relay dropping off the Zigbee mesh and
// coming back is not somebody flipping the switch, and neither is an
// attribute-only update. Either one toggling the light is the 3am bug.
func TestGaleriaStairsIgnoresNonPresses(t *testing.T) {
	h := newGaleriaHarness(t)

	h.report(galeriaSource, "on", "unavailable")
	h.report(galeriaSource, "unavailable", "on")
	h.report(galeriaSource, "on", "on")
	h.expectSilence()

	// The staircase relay's own state never feeds back into the hall switch.
	h.report(galeriaTarget, "off", "on")
	h.expectSilence()
}
