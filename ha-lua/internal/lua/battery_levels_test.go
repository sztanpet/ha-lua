package lua

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

// serveBatteryLevels boots the real examples/battery_levels.lua against a
// seeded tracker and returns its router. `series` plants stored samples before
// the first scan runs, which is what makes a drain rate testable without
// waiting days for one.
func serveBatteryLevels(t *testing.T, seed []ha.StateData, series map[string]any) *Router {
	t.Helper()
	dir := t.TempDir()
	copyRepoFile(t, filepath.Join(repoScriptsDir, "battery_levels.lua"), filepath.Join(dir, "battery_levels.lua"))
	copyRepoFile(t, filepath.Join(repoScriptsDir, "battery_levels.html"), filepath.Join(dir, "battery_levels.html"))

	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tracker := state.New(writeDB, readDB)
	kv := store.New(writeDB, readDB, "battery_levels")
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	router := NewRouter(reg)
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	if err := tracker.Seed(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	for key, value := range series {
		if err := kv.Set(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}

	r := NewRunner("battery_levels", dir, openTestRoot(t, dir), openTestRoot(t, t.TempDir()), tracker, sched, kv, global)
	reg.Add(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "battery_levels.lua")) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("battery_levels.lua did not finish loading")
	}
	router.Register("battery_levels", r.Routes())
	return router
}

// sample builds one stored series entry as the script writes it.
func sample(at time.Time, level float64) map[string]any {
	return map[string]any{"at": at.Unix(), "level": level}
}

type batteryRow struct {
	EntityID    string   `json:"entity_id"`
	Name        string   `json:"name"`
	Level       float64  `json:"level"`
	ChangedAgo  *float64 `json:"changed_ago"`
	DrainPerDay *float64 `json:"drain_per_day"`
	ETASeconds  *float64 `json:"eta_seconds"`
	ETAAtLeast  *float64 `json:"eta_at_least"`
	Samples     int      `json:"samples"`
	Steps       int      `json:"steps"`
	Ignored     bool     `json:"ignored"`
}

func getBatteries(t *testing.T, router *Router) []batteryRow {
	t.Helper()
	rec := doReqID(router, "battery_levels", "GET", "/api/state", "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/state status %d body %q", rec.Code, rec.Body.String())
	}
	var payload struct {
		Batteries []batteryRow `json:"batteries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return payload.Batteries
}

// batteryEvent is one line of an entity's debug trail.
type batteryEvent struct {
	At          float64  `json:"at"`
	Why         string   `json:"why"`
	From        *float64 `json:"from"`
	FromTier    string   `json:"from_tier"`
	FromETA     *float64 `json:"from_eta"`
	Low         *float64 `json:"low"`
	Wiped       *float64 `json:"wiped"`
	Trimmed     bool     `json:"trimmed"`
	Level       float64  `json:"level"`
	Samples     int      `json:"samples"`
	OldestAt    *float64 `json:"oldest_at"`
	OldestLevel *float64 `json:"oldest_level"`
	Span        *float64 `json:"span"`
	Drop        *float64 `json:"drop"`
	PerDay      *float64 `json:"per_day"`
	ETA         *float64 `json:"eta"`
	Floor       *float64 `json:"floor"`
}

type batteryDetail struct {
	EntityID string  `json:"entity_id"`
	Now      float64 `json:"now"`
	Level    float64 `json:"level"`
	Ignored  bool    `json:"ignored"`
	Series   []struct {
		At    float64 `json:"at"`
		Level float64 `json:"level"`
	} `json:"series"`
	Events []batteryEvent `json:"events"`
	Math   struct {
		MinSpan     float64  `json:"min_span"`
		Remaining   float64  `json:"remaining"`
		OldestAt    *float64 `json:"oldest_at"`
		OldestLevel *float64 `json:"oldest_level"`
		Span        *float64 `json:"span"`
		Drop        *float64 `json:"drop"`
		Low         *float64 `json:"low"`
		ResetsAt    *float64 `json:"resets_at"`
		PerDay      *float64 `json:"per_day"`
		ETASeconds  *float64 `json:"eta_seconds"`
		ETAAtLeast  *float64 `json:"eta_at_least"`
		Tier        string   `json:"tier"`
		Samples     int      `json:"samples"`
		Steps       int      `json:"steps"`
	} `json:"math"`
}

func getDetail(t *testing.T, router *Router, entityID string) batteryDetail {
	t.Helper()
	target := "/api/detail?entity_id=" + url.QueryEscape(entityID)
	rec := doReqID(router, "battery_levels", "GET", target, "")
	if rec.Code != 200 {
		t.Fatalf("GET %s status %d body %q", target, rec.Code, rec.Body.String())
	}
	var detail batteryDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return detail
}

// TestBatteryLevelsRundown checks the forecast and the ordering that is the
// whole point of the page: a battery draining fast must sort above a fuller one
// that lasts longer, and a straight 2%/day line must predict 2%/day.
func TestBatteryLevelsRundown(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		// 80%, dropping 2 points/day -> empty in ~40 days.
		{EntityID: "sensor.attic_battery", State: "80",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Attic"}`),
			LastChanged: now.Add(-6 * time.Hour).UTC().Format(time.RFC3339)},
		// 30%, dropping 10 points/day -> empty in ~3 days, so it leads.
		{EntityID: "sensor.doorbell_battery", State: "30",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Doorbell"}`),
			LastChanged: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.attic_battery": []any{
			sample(now.Add(-10*24*time.Hour), 100),
			sample(now.Add(-5*24*time.Hour), 90),
		},
		"series:sensor.doorbell_battery": []any{
			sample(now.Add(-5*24*time.Hour), 80),
			sample(now.Add(-2*24*time.Hour), 50),
		},
	}

	rows := getBatteries(t, serveBatteryLevels(t, seed, series))
	if len(rows) != 2 {
		t.Fatalf("got %d batteries, want 2: %+v", len(rows), rows)
	}
	if rows[0].EntityID != "sensor.doorbell_battery" {
		t.Errorf("first row = %s, want the doorbell (dies first)", rows[0].EntityID)
	}

	doorbell, attic := rows[0], rows[1]
	if doorbell.ETASeconds == nil || attic.ETASeconds == nil {
		t.Fatalf("missing ETA: %+v", rows)
	}
	if err := closeTo(*attic.DrainPerDay, 2, 0.2); err != nil {
		t.Errorf("attic drain: %v", err)
	}
	// Countdown runs to EMPTY_LEVEL (15%), not to zero: 65 points left at
	// 2/day, and 15 left at 10/day.
	if err := closeTo(*attic.ETASeconds/86400, 32.5, 2); err != nil {
		t.Errorf("attic ETA days: %v", err)
	}
	if err := closeTo(*doorbell.ETASeconds/86400, 1.5, 0.5); err != nil {
		t.Errorf("doorbell ETA days: %v", err)
	}
	// last_changed is reported from Home Assistant when the state IS the level.
	if doorbell.ChangedAgo == nil || *doorbell.ChangedAgo < 2*3600-60 {
		t.Errorf("doorbell changed_ago = %v, want ~2h", doorbell.ChangedAgo)
	}
}

// TestBatteryLevelsSingleStepAndFloor pins the three things a row can say: a
// forecast off one step, a floor for a battery that never stepped, and "due
// now" below EMPTY_LEVEL. Tiers beat numbers, so the measured 450 days must
// still outrank the 102-day floor.
func TestBatteryLevelsSingleStepAndFloor(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.hall_battery", State: "90",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Hall"}`),
			LastChanged: now.Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)},
		{EntityID: "sensor.shed_battery", State: "100",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Shed"}`),
			LastChanged: now.Add(-12 * 24 * time.Hour).UTC().Format(time.RFC3339)},
		{EntityID: "sensor.smoke_battery", State: "10",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Smoke"}`),
			LastChanged: now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		// One sighting at 100 sixty days ago; the state is 90 now, so the scan
		// records the step and the window is 60 days wide.
		"series:sensor.hall_battery": []any{sample(now.Add(-60*24*time.Hour), 100)},
	}

	rows := getBatteries(t, serveBatteryLevels(t, seed, series))
	if len(rows) != 3 {
		t.Fatalf("got %d batteries, want 3: %+v", len(rows), rows)
	}
	byID := map[string]batteryRow{}
	var order []string
	for _, row := range rows {
		byID[row.EntityID] = row
		order = append(order, row.EntityID)
	}
	want := []string{"sensor.smoke_battery", "sensor.hall_battery", "sensor.shed_battery"}
	for i, id := range want {
		if order[i] != id {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}

	hall := byID["sensor.hall_battery"]
	if hall.Steps != 1 || hall.Samples != 2 {
		t.Errorf("hall = %d step(s)/%d sample(s), want 1/2", hall.Steps, hall.Samples)
	}
	if hall.ETASeconds == nil {
		t.Fatalf("hall has no ETA from a single step: %+v", hall)
	}
	// 10 points over 60 days, 75 left above the 15% line.
	if err := closeTo(*hall.DrainPerDay, 10.0/60, 0.02); err != nil {
		t.Errorf("hall drain: %v", err)
	}
	if err := closeTo(*hall.ETASeconds/86400, 450, 10); err != nil {
		t.Errorf("hall ETA days: %v", err)
	}

	shed := byID["sensor.shed_battery"]
	if shed.ETASeconds != nil || shed.DrainPerDay != nil {
		t.Errorf("shed = %+v, want no rate from a battery that never moved", shed)
	}
	if shed.ETAAtLeast == nil {
		t.Fatalf("shed has no floor after 12 days at one level: %+v", shed)
	}
	// 85 points left, 12 days per assumed 10-point step.
	if err := closeTo(*shed.ETAAtLeast/86400, 102, 1); err != nil {
		t.Errorf("shed floor days: %v", err)
	}

	smoke := byID["sensor.smoke_battery"]
	if smoke.ETASeconds == nil || *smoke.ETASeconds != 0 {
		t.Errorf("smoke = %+v, want an ETA of 0 below EMPTY_LEVEL", smoke)
	}
}

// TestBatteryLevelsDiscovery covers what lands on the page and what does not:
// a battery_level attribute counts, an unrelated sensor does not, and an entity
// with no usable history is listed without an ETA rather than dropped.
func TestBatteryLevelsDiscovery(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "device_tracker.phone", State: "home",
			Attributes:  jsontext.Value(`{"battery_level":42,"friendly_name":"Phone"}`),
			LastChanged: now.Add(-30 * time.Minute).UTC().Format(time.RFC3339)},
		{EntityID: "sensor.kitchen_temperature", State: "21.5",
			Attributes: jsontext.Value(`{"device_class":"temperature"}`)},
		{EntityID: "sensor.remote_battery", State: "unavailable",
			Attributes: jsontext.Value(`{"device_class":"battery"}`)},
	}

	rows := getBatteries(t, serveBatteryLevels(t, seed, nil))
	if len(rows) != 1 {
		t.Fatalf("got %d batteries, want only the phone: %+v", len(rows), rows)
	}
	phone := rows[0]
	if phone.EntityID != "device_tracker.phone" || phone.Level != 42 {
		t.Errorf("row = %+v, want the phone at 42%%", phone)
	}
	if phone.ETASeconds != nil {
		t.Errorf("eta_seconds = %v on a first sighting, want none", *phone.ETASeconds)
	}
	// No usable last-change either, so there is not even a floor to state.
	if phone.ETAAtLeast != nil {
		t.Errorf("eta_at_least = %v on a first sighting, want none", *phone.ETAAtLeast)
	}
	// The tracker's state changes whenever the phone moves, so its last_changed
	// says nothing about the battery, and one sample is not a change either.
	if phone.ChangedAgo != nil {
		t.Errorf("changed_ago = %v on a first sighting, want none", *phone.ChangedAgo)
	}
	if phone.Samples != 1 {
		t.Errorf("samples = %d, want 1", phone.Samples)
	}
}

// TestBatteryLevelsRechargeResets confirms a charge throws the old run away:
// fitting across it would forecast from a slope that no longer exists.
func TestBatteryLevelsRechargeResets(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.vacuum_battery", State: "95",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Vacuum"}`),
			LastChanged: now.Add(-time.Minute).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.vacuum_battery": []any{
			sample(now.Add(-4*24*time.Hour), 60),
			sample(now.Add(-2*24*time.Hour), 40),
			sample(now.Add(-24*time.Hour), 20),
		},
	}

	rows := getBatteries(t, serveBatteryLevels(t, seed, series))
	if len(rows) != 1 {
		t.Fatalf("got %d batteries, want 1", len(rows))
	}
	if rows[0].Samples != 1 {
		t.Errorf("samples = %d after a recharge, want the series restarted at 1", rows[0].Samples)
	}
	if rows[0].ETASeconds != nil {
		t.Errorf("eta_seconds = %v across a recharge, want none", *rows[0].ETASeconds)
	}
	// The level moved a minute ago, so the dwell is too short to floor either.
	if rows[0].ETAAtLeast != nil {
		t.Errorf("eta_at_least = %v across a recharge, want none", *rows[0].ETAAtLeast)
	}
}

// TestBatteryLevelsNoisyLevelKeeps pins the reason the threshold is ten: a
// level that wobbles a few points a day must keep its run, or it re-starts
// every day and forecasts nothing for the rest of its life.
func TestBatteryLevelsNoisyLevelKeeps(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.attic_battery", State: "71",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Attic"}`),
			LastChanged: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.attic_battery": []any{
			sample(now.Add(-5*24*time.Hour), 80),
			sample(now.Add(-4*24*time.Hour), 77),
			sample(now.Add(-3*24*time.Hour), 79),
			sample(now.Add(-2*24*time.Hour), 74),
			sample(now.Add(-24*time.Hour), 76),
		},
	}

	rows := getBatteries(t, serveBatteryLevels(t, seed, series))
	if len(rows) != 1 {
		t.Fatalf("got %d batteries, want 1", len(rows))
	}
	if rows[0].Samples != 6 {
		t.Errorf("samples = %d, want the noisy run kept and extended to 6", rows[0].Samples)
	}
	if rows[0].ETASeconds == nil {
		t.Fatalf("eta_seconds = nil, want a forecast from the surviving run")
	}
	// 9 points over 5 days -> 1.8/day, 56 left above empty -> ~31 days.
	if days := *rows[0].ETASeconds / (24 * 60 * 60); days < 29 || days > 33 {
		t.Errorf("eta = %.1f days, want ~31", days)
	}
}

// TestBatteryLevelsSlowRechargeResets covers the charge that never jumps: a
// pack topped up a couple of points at a time trips no single-step threshold,
// but ten points above the run's low is a recharge however it got there.
func TestBatteryLevelsSlowRechargeResets(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.vacuum_battery", State: "50",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Vacuum"}`),
			LastChanged: now.Add(-time.Minute).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.vacuum_battery": []any{
			sample(now.Add(-4*24*time.Hour), 60),
			sample(now.Add(-2*24*time.Hour), 40),
			sample(now.Add(-6*time.Hour), 42),
			sample(now.Add(-4*time.Hour), 44),
			sample(now.Add(-2*time.Hour), 46),
			sample(now.Add(-time.Hour), 48),
		},
	}

	rows := getBatteries(t, serveBatteryLevels(t, seed, series))
	if len(rows) != 1 {
		t.Fatalf("got %d batteries, want 1", len(rows))
	}
	if rows[0].Samples != 1 {
		t.Errorf("samples = %d after a slow recharge, want the series restarted at 1", rows[0].Samples)
	}
	if rows[0].ETASeconds != nil {
		t.Errorf("eta_seconds = %v across a slow recharge, want none", *rows[0].ETASeconds)
	}
}

// TestBatteryLevelsDetailExposesMath pins the inspector: every input the
// forecast was derived from comes back with it, the numbers agree with the ones
// on the page, and asking does not sample — inspecting a suspect row must not
// alter the thing being inspected.
func TestBatteryLevelsDetailExposesMath(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.attic_battery", State: "71",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Attic"}`),
			LastChanged: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		{EntityID: "light.kitchen", State: "on",
			Attributes:  jsontext.Value(`{"friendly_name":"Kitchen"}`),
			LastChanged: now.Add(-time.Hour).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.attic_battery": []any{
			sample(now.Add(-5*24*time.Hour), 80),
			sample(now.Add(-4*24*time.Hour), 77),
			sample(now.Add(-3*24*time.Hour), 79),
			sample(now.Add(-2*24*time.Hour), 74),
			sample(now.Add(-24*time.Hour), 76),
		},
	}
	router := serveBatteryLevels(t, seed, series)

	rows := getBatteries(t, router)
	if rows[0].ETASeconds == nil {
		t.Fatalf("row = %+v, want a forecast to inspect", rows[0])
	}
	// A second scan changes nothing: a quiet poll must not fill the trail.
	getBatteries(t, router)

	detail := getDetail(t, router, "sensor.attic_battery")
	if len(detail.Series) != 6 {
		t.Fatalf("series = %d samples, want the stored 5 plus the scanned one", len(detail.Series))
	}
	if detail.Math.Tier != "eta" {
		t.Errorf("tier = %q, want %q", detail.Math.Tier, "eta")
	}
	// The whole forecast is these two numbers: 80 down to 71 over five days.
	if detail.Math.Drop == nil || *detail.Math.Drop != 9 {
		t.Errorf("drop = %v, want 9", detail.Math.Drop)
	}
	if detail.Math.OldestLevel == nil || *detail.Math.OldestLevel != 80 {
		t.Errorf("oldest_level = %v, want 80", detail.Math.OldestLevel)
	}
	if detail.Math.Span == nil || *detail.Math.Span < 5*24*3600-60 {
		t.Errorf("span = %v, want ~5 days", detail.Math.Span)
	}
	// The wipe threshold is measured from the run's low, which is now the
	// newest sample — the one number that explains a series vanishing.
	if detail.Math.Low == nil || *detail.Math.Low != 71 {
		t.Errorf("low = %v, want 71", detail.Math.Low)
	}
	if detail.Math.ResetsAt == nil || *detail.Math.ResetsAt != 81 {
		t.Errorf("resets_at = %v, want 81", detail.Math.ResetsAt)
	}
	if detail.Math.Remaining != 56 {
		t.Errorf("remaining = %v, want 56 above empty", detail.Math.Remaining)
	}
	if detail.Math.ETASeconds == nil {
		t.Fatalf("detail eta = nil, want the page's forecast")
	}
	if delta := *detail.Math.ETASeconds - *rows[0].ETASeconds; delta > 1 || delta < -1 {
		t.Errorf("detail eta %v disagrees with the page's %v", *detail.Math.ETASeconds, *rows[0].ETASeconds)
	}

	if len(detail.Events) != 1 {
		t.Fatalf("events = %d, want one for the sample the scan appended: %+v", len(detail.Events), detail.Events)
	}
	event := detail.Events[0]
	if event.Why != "sample" || event.From == nil || *event.From != 76 {
		t.Errorf("event = %+v, want a sample stepping down from 76", event)
	}
	if event.Samples != 6 || event.Drop == nil || *event.Drop != 9 {
		t.Errorf("event = %+v, want the whole computation recorded with it", event)
	}

	again := getDetail(t, router, "sensor.attic_battery")
	if len(again.Series) != 6 || len(again.Events) != 1 {
		t.Errorf("inspecting sampled: %d samples / %d events, want 6 / 1",
			len(again.Series), len(again.Events))
	}

	for _, tc := range []struct {
		target string
		want   int
	}{
		{"/api/detail", 400},
		{"/api/detail?entity_id=", 400},
		{"/api/detail?entity_id=sensor.nonexistent", 404},
		{"/api/detail?entity_id=light.kitchen", 404},
	} {
		if rec := doReqID(router, "battery_levels", "GET", tc.target, ""); rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.target, rec.Code, tc.want)
		}
	}
}

// TestBatteryLevelsTrailExplainsReset is the case the trail exists for: a
// recharge silently throws away the run behind a forecast, so the page loses
// its number with nothing on it to say why. The event survives the series.
func TestBatteryLevelsTrailExplainsReset(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.vacuum_battery", State: "95",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Vacuum"}`),
			LastChanged: now.Add(-time.Minute).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.vacuum_battery": []any{
			sample(now.Add(-4*24*time.Hour), 60),
			sample(now.Add(-2*24*time.Hour), 40),
			sample(now.Add(-24*time.Hour), 20),
		},
	}
	router := serveBatteryLevels(t, seed, series)

	if rows := getBatteries(t, router); rows[0].ETASeconds != nil {
		t.Fatalf("eta = %v across a recharge, want the forecast gone", *rows[0].ETASeconds)
	}

	detail := getDetail(t, router, "sensor.vacuum_battery")
	if len(detail.Series) != 1 {
		t.Fatalf("series = %d samples, want the run restarted", len(detail.Series))
	}
	if len(detail.Events) != 1 {
		t.Fatalf("events = %d, want the reset recorded: %+v", len(detail.Events), detail.Events)
	}
	event := detail.Events[0]
	if event.Why != "reset" {
		t.Errorf("why = %q, want %q", event.Why, "reset")
	}
	if event.Wiped == nil || *event.Wiped != 3 {
		t.Errorf("wiped = %v, want the 3 discarded samples counted", event.Wiped)
	}
	// 20 was the run's low and 95 cleared it by more than RECHARGE_RISE: the
	// two numbers that decided it, kept where they can still be read.
	if event.Low == nil || *event.Low != 20 {
		t.Errorf("low = %v, want 20", event.Low)
	}
	if event.Level != 95 {
		t.Errorf("level = %v, want the 95 that tripped it", event.Level)
	}
	if event.ETA != nil {
		t.Errorf("eta = %v, want none left after the wipe", *event.ETA)
	}
	if detail.Math.Tier != "none" {
		t.Errorf("tier = %q, want %q", detail.Math.Tier, "none")
	}
}

func TestBatteryLevelsIgnore(t *testing.T) {
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.attic_battery", State: "80",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Attic"}`),
			LastChanged: now.Add(-6 * time.Hour).UTC().Format(time.RFC3339)},
		{EntityID: "sensor.doorbell_battery", State: "30",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Doorbell"}`),
			LastChanged: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.doorbell_battery": []any{
			sample(now.Add(-5*24*time.Hour), 80),
			sample(now.Add(-2*24*time.Hour), 50),
		},
	}
	router := serveBatteryLevels(t, seed, series)

	if rows := getBatteries(t, router); rows[0].EntityID != "sensor.doorbell_battery" {
		t.Fatalf("first row = %s, want the doorbell", rows[0].EntityID)
	}

	rows := postIgnore(t, router, "sensor.doorbell_battery", true, 200)
	if len(rows) != 2 {
		t.Fatalf("got %d batteries after ignoring one, want both listed: %+v", len(rows), rows)
	}
	doorbell := rows[1]
	if doorbell.EntityID != "sensor.doorbell_battery" || !doorbell.Ignored {
		t.Fatalf("last row = %+v, want the ignored doorbell", doorbell)
	}
	if doorbell.ETASeconds != nil || doorbell.DrainPerDay != nil || doorbell.Samples != 0 {
		t.Errorf("ignored row = %+v, want no forecast and no samples", doorbell)
	}
	if doorbell.Level != 30 {
		t.Errorf("ignored level = %v, want the level still reported", doorbell.Level)
	}

	rows = postIgnore(t, router, "sensor.doorbell_battery", false, 200)
	if rows[0].EntityID != "sensor.doorbell_battery" || rows[0].Ignored {
		t.Fatalf("first row after tracking again = %+v, want the doorbell back on top", rows[0])
	}
	if rows[0].Samples != 1 || rows[0].ETASeconds != nil {
		t.Errorf("resumed row = %+v, want a series restarted at one sample", rows[0])
	}

	postIgnore(t, router, "sensor.nonexistent_battery", true, 404)
	if rec := doReqID(router, "battery_levels", "POST", "/api/ignore", `{"ignored":true}`); rec.Code != 400 {
		t.Errorf("POST without an entity_id = %d, want 400", rec.Code)
	}
}

func postIgnore(t *testing.T, router *Router, entityID string, ignored bool, wantCode int) []batteryRow {
	t.Helper()
	body := fmt.Sprintf(`{"entity_id":%q,"ignored":%t}`, entityID, ignored)
	rec := doReqID(router, "battery_levels", "POST", "/api/ignore", body)
	if rec.Code != wantCode {
		t.Fatalf("POST /api/ignore %s status %d, want %d (body %q)", body, rec.Code, wantCode, rec.Body.String())
	}
	if rec.Code != 200 {
		return nil
	}
	var payload struct {
		Batteries []batteryRow `json:"batteries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return payload.Batteries
}

// TestBatteryLevelsUIRendersRows drives the served page in a browser: the rows
// must arrive in the daemon's urgency order, carry the level and the forecast,
// and the sort control must reorder them client-side.
func TestBatteryLevelsUIRendersRows(t *testing.T) {
	ctx := newBrowserCtx(t)
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.attic_battery", State: "80",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Attic"}`),
			LastChanged: now.Add(-6 * time.Hour).UTC().Format(time.RFC3339)},
		{EntityID: "sensor.doorbell_battery", State: "30",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Doorbell"}`),
			LastChanged: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.attic_battery": []any{
			sample(now.Add(-10*24*time.Hour), 100),
			sample(now.Add(-5*24*time.Hour), 90),
		},
		"series:sensor.doorbell_battery": []any{
			sample(now.Add(-5*24*time.Hour), 80),
			sample(now.Add(-2*24*time.Hour), 50),
		},
	}
	srv := httptest.NewServer(serveBatteryLevels(t, seed, series))
	t.Cleanup(srv.Close)

	const names = `Array.from(document.querySelectorAll(".row .name")).map(node => node.firstChild.textContent)`
	var order []string
	var firstRow string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/s/battery_levels/"),
		chromedp.WaitVisible(".row .pct", chromedp.ByQuery),
		chromedp.Evaluate(names, &order),
		chromedp.Text(".row", &firstRow, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "Doorbell" {
		t.Errorf("row order = %v, want the doorbell first", order)
	}
	// 30% with 15 points left above the empty line, at 10 points/day.
	for _, want := range []string{"30%", "36 h"} {
		if !strings.Contains(firstRow, want) {
			t.Errorf("first row %q missing %q", firstRow, want)
		}
	}

	// Sorting by name is presentation only, so it must reorder without a fetch.
	if err := chromedp.Run(ctx,
		chromedp.SetValue("#sort", "name", chromedp.ByQuery),
		chromedp.Evaluate(names, &order),
	); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "Attic" {
		t.Errorf("name-sorted order = %v, want Attic first", order)
	}

	var buttons []string
	if err := chromedp.Run(ctx,
		chromedp.Click(".row .act button", chromedp.ByQuery),
		chromedp.WaitVisible(".row.ignored", chromedp.ByQuery),
		chromedp.Evaluate(names, &order),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".row .act button")).map(node => node.textContent)`, &buttons),
	); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[1] != "Attic" {
		t.Errorf("order after ignoring Attic = %v, want it listed last", order)
	}
	if len(buttons) != 2 || buttons[1] != "Track" {
		t.Errorf("row buttons = %v, want the ignored row offering Track", buttons)
	}
}

// TestBatteryLevelsUIRendersBounds: a floor and a measured ETA must not read as
// the same kind of number. The floor keeps its ">" and stays uncoloured; a rate
// read off a single step is hedged with a "~".
func TestBatteryLevelsUIRendersBounds(t *testing.T) {
	ctx := newBrowserCtx(t)
	now := time.Now()
	seed := []ha.StateData{
		{EntityID: "sensor.hall_battery", State: "90",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Hall"}`),
			LastChanged: now.Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)},
		{EntityID: "sensor.shed_battery", State: "100",
			Attributes:  jsontext.Value(`{"device_class":"battery","friendly_name":"Shed"}`),
			LastChanged: now.Add(-12 * 24 * time.Hour).UTC().Format(time.RFC3339)},
	}
	series := map[string]any{
		"series:sensor.hall_battery": []any{sample(now.Add(-60*24*time.Hour), 100)},
	}
	srv := httptest.NewServer(serveBatteryLevels(t, seed, series))
	t.Cleanup(srv.Close)

	const pills = `Array.from(document.querySelectorAll(".row .eta .pill")).map(node => node.textContent)`
	var texts []string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/s/battery_levels/"),
		chromedp.WaitVisible(".row .eta .pill", chromedp.ByQuery),
		chromedp.Evaluate(pills, &texts),
	); err != nil {
		t.Fatal(err)
	}
	// Hall: 75 points left at 10/60 per day. Shed: 85 left, 12 days per step.
	want := []string{"~15 mo", "> 3 mo"}
	if len(texts) != 2 || texts[0] != want[0] || texts[1] != want[1] {
		t.Errorf("eta pills = %v, want %v", texts, want)
	}
}

func closeTo(got, want, tolerance float64) error {
	if diff := got - want; diff < -tolerance || diff > tolerance {
		return fmt.Errorf("got %.3f, want %.3f ±%.3f", got, want, tolerance)
	}
	return nil
}
