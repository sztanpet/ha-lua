package lua

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
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
	Samples     int      `json:"samples"`
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
	if err := closeTo(*attic.ETASeconds/86400, 40, 2); err != nil {
		t.Errorf("attic ETA days: %v", err)
	}
	if err := closeTo(*doorbell.ETASeconds/86400, 3, 0.5); err != nil {
		t.Errorf("doorbell ETA days: %v", err)
	}
	// last_changed is reported from Home Assistant when the state IS the level.
	if doorbell.ChangedAgo == nil || *doorbell.ChangedAgo < 2*3600-60 {
		t.Errorf("doorbell changed_ago = %v, want ~2h", doorbell.ChangedAgo)
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
	for _, want := range []string{"30%", "3 d"} {
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

func closeTo(got, want, tolerance float64) error {
	if diff := got - want; diff < -tolerance || diff > tolerance {
		return fmt.Errorf("got %.3f, want %.3f ±%.3f", got, want, tolerance)
	}
	return nil
}
