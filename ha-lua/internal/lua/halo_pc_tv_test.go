package lua

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

const (
	pcTvPC = "binary_sensor.bedroom_halo_pc"
	pcTvTV = "media_player.bedroom_tv"
	// The shipped RETRY_EVERY is seconds; the copy under test waits this long.
	pcTvRetry    = 150 * time.Millisecond
	pcTvAttempts = 5
)

// pcTvHarness runs the real examples/halo_pc_tv.lua against a spy call
// service and a live scheduler, since the retries are ha.after timers.
type pcTvHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	cmds    chan string  // "turn_on"
	fail    atomic.Int32 // the next this-many calls raise, like a TV in deep standby
}

func newPcTvHarness(t *testing.T, pc, tv string) *pcTvHarness {
	dir := t.TempDir()
	script := filepath.Join(dir, "halo_pc_tv.lua")
	copyRepoFile(t, filepath.Join(repoScriptsDir, "halo_pc_tv.lua"), script)
	shortenPcTvRetry(t, script)

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	tracker.Start(t.Context())
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	ctx, cancel := context.WithCancel(context.Background())
	h := &pcTvHarness{t: t, ctx: ctx, tracker: tracker, reg: reg,
		cmds: make(chan string, 16)}

	if err := tracker.Seed(ctx, []ha.StateData{
		seedEntity(pcTvPC, pc, `{}`),
		seedEntity(pcTvTV, tv, `{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("halo_pc_tv", dir, openTestRoot(t, dir), openTestRoot(t, t.TempDir()),
		tracker, sched, store.New(writeDB, readDB, "halo_pc_tv"), store.NewGlobal(writeDB, readDB))
	r.SetCallService(func(_ context.Context, domain, service string, data jsontext.Value) error {
		var payload struct {
			EntityID any `json:"entity_id"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("bad service data %s: %v", data, err)
		}
		if got := domain + " " + strings.Join(entityIDs(payload.EntityID), ","); got != "media_player "+pcTvTV {
			t.Errorf("call targets %q, want media_player %s", got, pcTvTV)
		}
		h.cmds <- service
		if h.fail.Add(-1) >= 0 {
			return errors.New("Android TV is not connected")
		}
		return nil
	})
	reg.Add(r)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, script) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("halo_pc_tv.lua did not finish loading")
	}
	return h
}

// shortenPcTvRetry asserts the shipped line is there, so renaming the option
// breaks the test loudly instead of waiting out the real interval.
func shortenPcTvRetry(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const shipped = `local RETRY_EVERY = "20s"`
	if !strings.Contains(string(src), shipped) {
		t.Fatalf("%q not found in the example — was the option renamed?", shipped)
	}
	out := strings.Replace(string(src), shipped, `local RETRY_EVERY = "`+pcTvRetry.String()+`"`, 1)
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *pcTvHarness) report(entityID, oldState, newState string) {
	h.t.Helper()
	entity := func(stateVal string) string {
		return `{"entity_id":"` + entityID + `","state":"` + stateVal +
			`","attributes":{},"last_changed":"2026-10-12T01:00:00Z",` +
			`"last_updated":"2026-10-12T01:00:00Z"}`
	}
	raw := jsontext.Value(`{"entity_id":"` + entityID + `","old_state":` +
		entity(oldState) + `,"new_state":` + entity(newState) + `}`)
	if err := h.tracker.HandleStateChanged(h.ctx, raw); err != nil {
		h.t.Fatal(err)
	}
	h.reg.Dispatch(ha.Event{Type: "state_changed", Data: raw})
}

func (h *pcTvHarness) expectCmd(want string) {
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

// expectSilence outlasts two retry intervals, so a retry that should not
// happen has time to.
func (h *pcTvHarness) expectSilence() {
	h.t.Helper()
	select {
	case got := <-h.cmds:
		h.t.Fatalf("unexpected service call %q", got)
	case <-time.After(2*pcTvRetry + 100*time.Millisecond):
	}
}

func TestPcTvFollowsPC(t *testing.T) {
	h := newPcTvHarness(t, "off", "off")

	h.report(pcTvPC, "off", "on")
	h.expectCmd("turn_on")
	h.report(pcTvTV, "off", "on")
	h.expectSilence()

	h.report(pcTvPC, "on", "off")
	h.expectCmd("turn_off")
	h.report(pcTvTV, "on", "off")
	h.expectSilence()
}

// TestPcTvRetriesUntilTvReports: a call that goes through but leaves the TV
// off is not done — only the TV's own state is.
func TestPcTvRetriesUntilTvReports(t *testing.T) {
	h := newPcTvHarness(t, "off", "off")

	h.report(pcTvPC, "off", "on")
	h.expectCmd("turn_on")
	h.expectCmd("turn_on")
	h.report(pcTvTV, "off", "on")
	h.expectSilence()
}

// TestPcTvRetriesFailedCall: a TV in deep standby has no remote connection,
// so HA raises; that must not end the retries.
func TestPcTvRetriesFailedCall(t *testing.T) {
	h := newPcTvHarness(t, "on", "unavailable")
	h.fail.Store(2)

	h.report(pcTvPC, "on", "off")
	h.expectCmd("turn_off")
	h.expectCmd("turn_off")
	h.expectCmd("turn_off")
	h.report(pcTvTV, "unavailable", "off")
	h.expectSilence()
}

func TestPcTvGivesUp(t *testing.T) {
	h := newPcTvHarness(t, "off", "off")

	h.report(pcTvPC, "off", "on")
	for range pcTvAttempts {
		h.expectCmd("turn_on")
	}
	h.expectSilence()
}

// TestPcTvNewEdgeCancelsRetries: the PC gone again before the TV came on must
// not leave the old chain pressing POWER — it would switch the TV on.
func TestPcTvNewEdgeCancelsRetries(t *testing.T) {
	h := newPcTvHarness(t, "off", "off")

	h.report(pcTvPC, "off", "on")
	h.expectCmd("turn_on")
	h.report(pcTvPC, "on", "off")
	h.expectSilence()
}

// TestPcTvIgnoresUnavailable: an HA restart takes the ping sensor through
// unavailable; that is not the PC switching on or off.
func TestPcTvIgnoresUnavailable(t *testing.T) {
	h := newPcTvHarness(t, "on", "on")

	h.report(pcTvPC, "on", "unavailable")
	h.report(pcTvPC, "unavailable", "on")
	h.report(pcTvPC, "unavailable", "off")
	h.expectSilence()
}
