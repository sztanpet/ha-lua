package lua

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net"
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
	pcTvTV   = "media_player.bedroom_tv"
	pcTvLink = "binary_sensor.ether3_connectivity"
	// The shipped intervals are seconds; the copy under test uses these.
	pcTvProbe    = 20 * time.Millisecond
	pcTvRetry    = 150 * time.Millisecond
	pcTvAttempts = 5
	pcTvSettle   = 100 * time.Millisecond
	pcTvGrace    = 300 * time.Millisecond
)

// pcTvHarness runs the real examples/halo_pc_tv.lua against a spy call
// service, a live scheduler (the probe and the retries are timers) and a
// local listener standing in for the PC.
type pcTvHarness struct {
	t       *testing.T
	ctx     context.Context
	tracker *state.Tracker
	reg     *Registry
	pc      *fakePC
	cmds    chan string  // "turn_on"
	fail    atomic.Int32 // the next this-many calls raise, like a TV in deep standby
}

// fakePC answers net.reachable on a fixed port while it is on, and refuses it
// while it is off.
type fakePC struct {
	t    *testing.T
	addr string
	ln   net.Listener
}

func newFakePC(t *testing.T, on bool) *fakePC {
	pc := &fakePC{t: t}
	pc.listen("127.0.0.1:0")
	pc.addr = pc.ln.Addr().String()
	if !on {
		pc.off()
	}
	t.Cleanup(func() {
		if pc.ln != nil {
			pc.ln.Close()
		}
	})
	return pc
}

func (pc *fakePC) listen(addr string) {
	ln, err := (&net.ListenConfig{}).Listen(pc.t.Context(), "tcp", addr)
	if err != nil {
		pc.t.Fatal(err)
	}
	pc.ln = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
}

func (pc *fakePC) on() { pc.listen(pc.addr) }

func (pc *fakePC) off() {
	pc.ln.Close()
	pc.ln = nil
}

func newPcTvHarness(t *testing.T, pcOn bool, tv string) *pcTvHarness {
	pc := newFakePC(t, pcOn)
	dir := t.TempDir()
	script := filepath.Join(dir, "halo_pc_tv.lua")
	copyRepoFile(t, filepath.Join(repoScriptsDir, "halo_pc_tv.lua"), script)
	rewriteShipped(t, script, map[string]string{
		`local PC = "192.168.1.12:445"`: `local PC = "` + pc.addr + `"`,
		`local PROBE_EVERY = "2s"`:      `local PROBE_EVERY = "` + pcTvProbe.String() + `"`,
		`local RETRY_EVERY = "20s"`:     `local RETRY_EVERY = "` + pcTvRetry.String() + `"`,
		`time.parse_duration("60s")`:    `time.parse_duration("` + pcTvSettle.String() + `")`,
		`local BOOT_GRACE = "2m"`:       `local BOOT_GRACE = "` + pcTvGrace.String() + `"`,
	})

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	tracker.Start(t.Context())
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	ctx, cancel := context.WithCancel(context.Background())
	h := &pcTvHarness{t: t, ctx: ctx, tracker: tracker, reg: reg, pc: pc,
		cmds: make(chan string, 16)}

	if err := tracker.Seed(ctx, []ha.StateData{
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

// rewriteShipped asserts each shipped line is there, so renaming an option
// breaks the test loudly instead of waiting out the real interval.
func rewriteShipped(t *testing.T, path string, lines map[string]string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(src)
	for shipped, replacement := range lines {
		if !strings.Contains(out, shipped) {
			t.Fatalf("%q not found in the example — was the option renamed?", shipped)
		}
		out = strings.Replace(out, shipped, replacement, 1)
	}
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
	h := newPcTvHarness(t, false, "off")
	h.expectSilence()

	h.pc.on()
	h.expectCmd("turn_on")
	h.report(pcTvTV, "off", "on")
	h.expectSilence()

	h.pc.off()
	h.expectCmd("turn_off")
	h.report(pcTvTV, "on", "off")
	h.expectSilence()
}

// TestPcTvRetriesUntilTvReports: a call that goes through but leaves the TV
// off is not done — only the TV's own state is.
func TestPcTvRetriesUntilTvReports(t *testing.T) {
	h := newPcTvHarness(t, false, "off")
	h.expectSilence()

	h.pc.on()
	h.expectCmd("turn_on")
	h.expectCmd("turn_on")
	h.report(pcTvTV, "off", "on")
	h.expectSilence()
}

// TestPcTvRetriesFailedCall: a TV in deep standby has no remote connection,
// so HA raises; that must not end the retries.
func TestPcTvRetriesFailedCall(t *testing.T) {
	h := newPcTvHarness(t, true, "unavailable")
	h.expectSilence()
	h.fail.Store(2)

	h.pc.off()
	h.expectCmd("turn_off")
	h.expectCmd("turn_off")
	h.expectCmd("turn_off")
	h.report(pcTvTV, "unavailable", "off")
	h.expectSilence()
}

func TestPcTvGivesUp(t *testing.T) {
	h := newPcTvHarness(t, false, "off")
	h.expectSilence()

	h.pc.on()
	for range pcTvAttempts {
		h.expectCmd("turn_on")
	}
	h.expectSilence()
}

// TestPcTvNewEdgeCancelsRetries: the PC gone again before the TV came on must
// not leave the old chain pressing POWER — it would switch the TV on.
func TestPcTvNewEdgeCancelsRetries(t *testing.T) {
	h := newPcTvHarness(t, false, "off")
	h.expectSilence()

	h.pc.on()
	h.expectCmd("turn_on")
	h.pc.off()
	h.expectSilence()
}

// TestPcTvFirstVerdictIsNotAnEdge: a restart or reload finding the PC on is
// not the PC switching on.
func TestPcTvFirstVerdictIsNotAnEdge(t *testing.T) {
	h := newPcTvHarness(t, true, "off")
	h.expectSilence()
}

// TestPcTvRidesOutAMissedProbe: an outage shorter than two probe intervals
// misses at most two probes, under OFF_AFTER_MISSES.
func TestPcTvRidesOutAMissedProbe(t *testing.T) {
	h := newPcTvHarness(t, true, "on")
	h.expectSilence()

	h.pc.off()
	time.Sleep(pcTvProbe + pcTvProbe/4)
	h.pc.on()
	h.expectSilence()
}

// TestPcTvWakesOnLinkDrop: the NIC renegotiating at power-on turns the TV on
// long before the PC answers the probe.
func TestPcTvWakesOnLinkDrop(t *testing.T) {
	h := newPcTvHarness(t, false, "off")
	h.expectSilence()

	h.report(pcTvLink, "on", "off")
	h.expectCmd("turn_on")
	h.report(pcTvTV, "off", "on")
	h.pc.on()
	time.Sleep(pcTvGrace)
	h.expectSilence()
}

func TestPcTvLinkDropWithoutBootReverts(t *testing.T) {
	h := newPcTvHarness(t, false, "off")
	h.expectSilence()

	h.report(pcTvLink, "on", "off")
	h.expectCmd("turn_on")
	h.report(pcTvTV, "off", "on")
	h.expectCmd("turn_off")
	h.report(pcTvTV, "on", "off")
	h.expectSilence()
}

// TestPcTvIgnoresLinkDropAtShutdown: a drop seconds after the PC went away is
// it shutting down or rebooting, not booting.
func TestPcTvIgnoresLinkDropAtShutdown(t *testing.T) {
	h := newPcTvHarness(t, true, "on")
	h.expectSilence()

	h.pc.off()
	h.expectCmd("turn_off")
	h.report(pcTvTV, "on", "off")
	h.report(pcTvLink, "on", "off")
	h.expectSilence()
}

func TestPcTvIgnoresLinkDropWhileOnline(t *testing.T) {
	h := newPcTvHarness(t, true, "on")
	h.expectSilence()

	h.report(pcTvLink, "on", "off")
	h.expectSilence()
}
