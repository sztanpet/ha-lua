package lua

import (
	"context"
	"database/sql"
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

const (
	seasonAverage = "sensor.kinti_3_napos_atlag"
	seasonOffAt   = "heating_season|at|10:00|1"
	seasonHeat    = "heat climate.konyha_furdo_futes,climate.konyha_nappali_futes," +
		"climate.konyha_halo_futes,climate.konyha_gyerekszoba_futes"
	seasonOff = "off climate.konyha_furdo_futes,climate.konyha_nappali_futes," +
		"climate.konyha_halo_futes,climate.konyha_gyerekszoba_futes"
)

// seasonHarness runs the real examples/heating_season.lua against a spy call
// service. The databases outlive restart(), which is how the armed flag is
// shown to survive one.
type seasonHarness struct {
	t               *testing.T
	dir             string
	writeDB, readDB *sql.DB
	tracker         *state.Tracker
	reg             *Registry
	ctx             context.Context
	stop            func()
	modes           chan string // "heat climate.a,climate.b"
}

func newSeasonHarness(t *testing.T, average string) *seasonHarness {
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "heating_season.lua"),
		filepath.Join(dir, "heating_season.lua"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	tracker.Start(t.Context())
	if err := tracker.Seed(t.Context(), []ha.StateData{
		seedEntity(seasonAverage, average, `{}`),
	}); err != nil {
		t.Fatal(err)
	}
	h := &seasonHarness{t: t, dir: dir, writeDB: writeDB, readDB: readDB,
		tracker: tracker, modes: make(chan string, 8)}
	h.start()
	return h
}

func (h *seasonHarness) start() {
	t := h.t
	h.reg = NewRegistry()
	sched := scheduler.New(h.writeDB, time.UTC, h.reg.DispatchToTimer)
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("heating_season", h.dir, openTestRoot(t, h.dir), openTestRoot(t, t.TempDir()),
		h.tracker, sched, store.New(h.writeDB, h.readDB, "heating_season"),
		store.NewGlobal(h.writeDB, h.readDB))
	r.SetCallService(func(_ context.Context, domain, service string, data jsontext.Value) error {
		var payload struct {
			EntityID any    `json:"entity_id"`
			HVACMode string `json:"hvac_mode"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("bad service data %s: %v", data, err)
		}
		if domain+"."+service != "climate.set_hvac_mode" {
			t.Errorf("unexpected service %s.%s", domain, service)
		}
		h.modes <- payload.HVACMode + " " + strings.Join(entityIDs(payload.EntityID), ",")
		return nil
	})
	h.reg.Add(r)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(h.dir, "heating_season.lua")) }()
	h.stop = func() { cancel(); <-done }
	t.Cleanup(func() { h.stop() })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("heating_season.lua did not finish loading")
	}
}

// restart waits for the handler to persist the flag it set, so the stop cannot
// cancel that write under it.
func (h *seasonHarness) restart(armed bool) {
	h.t.Helper()
	kv := store.New(h.writeDB, h.readDB, "heating_season")
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := kv.Get(h.t.Context(), "off_armed")
		if err != nil {
			h.t.Fatal(err)
		}
		if got == armed {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("off_armed = %v, want %v", got, armed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.stop()
	h.start()
}

func (h *seasonHarness) report(oldAverage, newAverage string) {
	h.t.Helper()
	entity := func(stateVal string) string {
		return `{"entity_id":"` + seasonAverage + `","state":"` + stateVal +
			`","attributes":{},"last_changed":"2026-10-10T01:00:00Z",` +
			`"last_updated":"2026-10-10T01:00:00Z"}`
	}
	raw := jsontext.Value(`{"entity_id":"` + seasonAverage + `","old_state":` +
		entity(oldAverage) + `,"new_state":` + entity(newAverage) + `}`)
	if err := h.tracker.HandleStateChanged(h.ctx, raw); err != nil {
		h.t.Fatal(err)
	}
	h.reg.Dispatch(ha.Event{Type: "state_changed", Data: raw})
}

func (h *seasonHarness) morning() {
	h.reg.DispatchToTimer("heating_season", seasonOffAt)
}

func (h *seasonHarness) expectMode(want string) {
	h.t.Helper()
	select {
	case got := <-h.modes:
		if got != want {
			h.t.Fatalf("set_hvac_mode = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		h.t.Fatalf("no set_hvac_mode, want %q", want)
	}
}

func (h *seasonHarness) expectSilence() {
	h.t.Helper()
	select {
	case got := <-h.modes:
		h.t.Fatalf("unexpected set_hvac_mode %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestHeatingSeasonSwitchesOffOncePerSeason is why the script exists: after
// the one armed switch-off, heating turned back on by hand survives every
// warm morning until the average has been below 14 again.
func TestHeatingSeasonSwitchesOffOncePerSeason(t *testing.T) {
	h := newSeasonHarness(t, "15.2")
	h.expectSilence()

	h.report("15.2", "13.9")
	h.expectMode(seasonHeat)
	h.report("13.9", "13.5")
	h.expectSilence()

	h.report("13.5", "17.4")
	h.morning()
	h.expectMode(seasonOff)

	h.morning()
	h.expectSilence()

	h.report("17.4", "13.8")
	h.expectMode(seasonHeat)
}

// TestHeatingSeasonWaitsForAWarmMorning: armed, but not off while the average
// is still at or under 17.
func TestHeatingSeasonWaitsForAWarmMorning(t *testing.T) {
	h := newSeasonHarness(t, "15")
	h.report("15", "13")
	h.expectMode(seasonHeat)

	h.report("13", "17")
	h.morning()
	h.expectSilence()

	h.report("17", "17.1")
	h.morning()
	h.expectMode(seasonOff)
}

// TestHeatingSeasonNeverArmedNeverSwitchesOff: an install that starts in a
// warm spell has no switch-off owed, so the morning leaves heating alone.
func TestHeatingSeasonNeverArmedNeverSwitchesOff(t *testing.T) {
	h := newSeasonHarness(t, "18.7")
	h.morning()
	h.expectSilence()
}

// TestHeatingSeasonCatchesUpAtLoad: a drop below 14 while the daemon was down
// still turns heat on, once.
func TestHeatingSeasonCatchesUpAtLoad(t *testing.T) {
	h := newSeasonHarness(t, "12.5")
	h.expectMode(seasonHeat)
	h.restart(true)
	h.expectSilence()
}

// TestHeatingSeasonArmedSurvivesRestart: the owed switch-off is in the store,
// not in a Lua local.
func TestHeatingSeasonArmedSurvivesRestart(t *testing.T) {
	h := newSeasonHarness(t, "15")
	h.report("15", "13")
	h.expectMode(seasonHeat)

	h.restart(true)
	h.report("13", "17.5")
	h.morning()
	h.expectMode(seasonOff)
}

// TestHeatingSeasonIgnoresUnavailable: a sensor dropping out reads as no
// average, not as a cold one.
func TestHeatingSeasonIgnoresUnavailable(t *testing.T) {
	h := newSeasonHarness(t, "15")
	h.report("15", "unavailable")
	h.expectSilence()
}
