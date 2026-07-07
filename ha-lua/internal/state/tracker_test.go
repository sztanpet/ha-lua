package state

import (
	"context"
	"testing"
	"time"

	"github.com/go-json-experiment/json/jsontext"
	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

func newTracker(t *testing.T) *Tracker {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	tr := New(writeDB, readDB)
	tr.Start(t.Context())
	return tr
}

func TestSeedAndGetState(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	states := []ha.StateData{
		{EntityID: "light.test", State: "on", Attributes: jsontext.Value(`{"brightness":200}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	}
	if err := tr.Seed(ctx, states); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := tr.GetState(ctx, "light.test")
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	if s == nil {
		t.Fatal("expected state, got nil")
	}
	if s.State != "on" {
		t.Errorf("state: want on, got %q", s.State)
	}
}

func TestSeedSkipsUnchangedHistory(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	states := []ha.StateData{
		{EntityID: "light.test", State: "on", Attributes: jsontext.Value(`{"brightness":200}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
		{EntityID: "sensor.temp", State: "21", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	}
	if err := tr.Seed(ctx, states); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	historyCount := func(entity string) int {
		tr.Flush() // seed appends are write-behind
		h, err := tr.GetHistory(ctx, entity, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(h)
	}

	if n := historyCount("light.test"); n != 1 {
		t.Fatalf("history after first seed: want 1, got %d", n)
	}

	// Re-seed with identical states: no new history rows (reconnect case).
	if err := tr.Seed(ctx, states); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if n := historyCount("light.test"); n != 1 {
		t.Errorf("history after identical re-seed: want 1, got %d", n)
	}

	// Re-seed with a changed state: exactly one new history row.
	states[0].State = "off"
	states[0].LastChanged = "2026-01-01T02:00:00Z"
	if err := tr.Seed(ctx, states); err != nil {
		t.Fatalf("third seed: %v", err)
	}
	if n := historyCount("light.test"); n != 2 {
		t.Errorf("history after changed re-seed: want 2, got %d", n)
	}
	if n := historyCount("sensor.temp"); n != 1 {
		t.Errorf("history for unchanged entity: want 1, got %d", n)
	}

	// The mirror still reflects the latest seed.
	s, err := tr.GetState(ctx, "light.test")
	if err != nil || s == nil {
		t.Fatalf("get_state: %v, %v", s, err)
	}
	if s.State != "off" {
		t.Errorf("mirror state: want off, got %q", s.State)
	}
}

// TestSeedDedupAcrossRestart: a fresh tracker over an existing database (a
// daemon restart — memory empty, history persisted) must dedup its first
// seed against the newest history row per entity, not append a phantom row
// for every entity in the home.
func TestSeedDedupAcrossRestart(t *testing.T) {
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	states := []ha.StateData{
		{EntityID: "light.test", State: "on", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	}
	before := New(writeDB, readDB)
	before.Start(t.Context())
	if err := before.Seed(ctx, states); err != nil {
		t.Fatalf("seed before restart: %v", err)
	}
	before.Flush() // the "restart" below must find the appends on disk

	// "Restart": a new tracker over the same database.
	after := New(writeDB, readDB)
	after.Start(t.Context())
	if err := after.Seed(ctx, states); err != nil {
		t.Fatalf("seed after restart: %v", err)
	}
	after.Flush()
	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := after.GetHistory(ctx, "light.test", since, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 {
		t.Errorf("identical seed across restart: want 1 history row, got %d", len(h))
	}

	// A state that really changed while the daemon was down gets its row.
	states[0].State = "off"
	states[0].LastChanged = "2026-01-01T02:00:00Z"
	again := New(writeDB, readDB)
	again.Start(t.Context())
	if err := again.Seed(ctx, states); err != nil {
		t.Fatalf("changed seed after restart: %v", err)
	}
	again.Flush()
	if h, err = again.GetHistory(ctx, "light.test", since, 100); err != nil || len(h) != 2 {
		t.Errorf("changed seed across restart: want 2 history rows, got %d (%v)", len(h), err)
	}
}

// TestSeedDeletesGhostEntities covers an entity removed from HA while the
// daemon was disconnected: it sends no nil-new_state event, so the re-seed is
// the only place the removal is visible. The mirror must drop it; its history
// stays. An empty batch must delete nothing (get_states never legitimately
// returns zero states).
func TestSeedDeletesGhostEntities(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	if err := tr.Seed(ctx, []ha.StateData{
		{EntityID: "light.kept", State: "on", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
		{EntityID: "automation.removed", State: "on", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	}); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	// Reconnect seed no longer carries the removed entity.
	if err := tr.Seed(ctx, []ha.StateData{
		{EntityID: "light.kept", State: "on", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	}); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	if s, err := tr.GetState(ctx, "automation.removed"); err != nil || s != nil {
		t.Errorf("ghost entity still in mirror: %+v, %v", s, err)
	}
	if s, err := tr.GetState(ctx, "light.kept"); err != nil || s == nil {
		t.Errorf("kept entity missing from mirror: %+v, %v", s, err)
	}
	tr.Flush() // seed appends are write-behind
	history, err := tr.GetHistory(ctx, "automation.removed", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Error("ghost deletion wiped state_history; want past states preserved")
	}

	// An empty batch must not be treated as "all entities removed".
	if err := tr.Seed(ctx, nil); err != nil {
		t.Fatalf("empty seed: %v", err)
	}
	if s, err := tr.GetState(ctx, "light.kept"); err != nil || s == nil {
		t.Errorf("empty seed wiped the mirror: %+v, %v", s, err)
	}
}

func TestHandleStateChanged(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	raw := jsontext.Value(`{
		"entity_id": "sensor.temp",
		"new_state": {"entity_id":"sensor.temp","state":"21","attributes":{},"last_changed":"2026-01-02T00:00:00Z","last_updated":"2026-01-02T00:00:00Z"},
		"old_state": {"entity_id":"sensor.temp","state":"20","attributes":{},"last_changed":"2026-01-01T00:00:00Z","last_updated":"2026-01-01T00:00:00Z"}
	}`)
	if err := tr.HandleStateChanged(ctx, raw); err != nil {
		t.Fatalf("handle: %v", err)
	}

	s, err := tr.GetState(ctx, "sensor.temp")
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "21" {
		t.Errorf("state after update: want 21, got %q", s.State)
	}
}

// TestHandleStateChangedRemoval covers HA deleting an entity: a state_changed
// with a nil new_state must drop the row from the current-state mirror (so
// GetState reports it gone) while leaving its state_history intact.
func TestHandleStateChangedRemoval(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	_ = tr.Seed(ctx, []ha.StateData{
		{EntityID: "automation.foo", State: "on", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	})

	// HA deletes the automation: new_state is nil, only old_state is present.
	if err := tr.HandleStateChanged(ctx, jsontext.Value(`{
		"entity_id": "automation.foo",
		"old_state": {"entity_id":"automation.foo","state":"on","attributes":{},"last_changed":"2026-01-01T00:00:00Z","last_updated":"2026-01-01T00:00:00Z"}
	}`)); err != nil {
		t.Fatalf("handle removal: %v", err)
	}

	s, err := tr.GetState(ctx, "automation.foo")
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Errorf("mirror still has removed entity: %+v", s)
	}

	// The seeded state happened; history must still carry it.
	tr.Flush() // seed appends are write-behind
	history, err := tr.GetHistory(ctx, "automation.foo", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Error("removal wiped state_history; want past states preserved")
	}
}

func TestStateHistoryAppended(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	// Seed initial state
	_ = tr.Seed(ctx, []ha.StateData{
		{EntityID: "light.living", State: "off", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	})

	// Update state
	_ = tr.HandleStateChanged(ctx, jsontext.Value(`{
		"entity_id": "light.living",
		"new_state": {"entity_id":"light.living","state":"on","attributes":{},"last_changed":"2026-01-01T01:00:00Z","last_updated":"2026-01-01T01:00:00Z"}
	}`))

	// History appends are write-behind; Flush is the test barrier.
	tr.Flush()
	history, err := tr.GetHistory(ctx, "light.living", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) < 2 {
		t.Errorf("expected at least 2 history rows, got %d", len(history))
	}
}

// TestGetHistorySinceTimezone guards the old foot gun: `since` used to be a
// raw string compared lexically, so a non-UTC instant ("…+02:00") sorted
// wrong against the UTC changed_at and silently dropped rows. A time.Time in
// any zone must now select by the actual instant.
func TestGetHistorySinceTimezone(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	_ = tr.Seed(ctx, []ha.StateData{
		{EntityID: "sensor.temp", State: "21", Attributes: jsontext.Value(`{}`),
			LastChanged: "2026-01-01T12:00:00Z", LastUpdated: "2026-01-01T12:00:00Z"},
	})
	tr.Flush() // seed appends are write-behind

	// 14:00+02:00 is 12:00Z — the same instant as the row, so it must match.
	plus2 := time.FixedZone("+02:00", 2*3600)
	since := time.Date(2026, 1, 1, 14, 0, 0, 0, plus2)
	h, err := tr.GetHistory(ctx, "sensor.temp", since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 {
		t.Errorf("since at the same instant in +02:00: want 1 row, got %d", len(h))
	}

	// One second later there is no row at or after it.
	h, err = tr.GetHistory(ctx, "sensor.temp", since.Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 0 {
		t.Errorf("since one second after the row: want 0 rows, got %d", len(h))
	}
}

func TestGetEntities(t *testing.T) {
	tr := newTracker(t)
	ctx := context.Background()

	_ = tr.Seed(ctx, []ha.StateData{
		{EntityID: "light.bedroom", State: "on", Attributes: jsontext.Value(`{}`), LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
		{EntityID: "light.kitchen", State: "off", Attributes: jsontext.Value(`{}`), LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
		{EntityID: "sensor.temp", State: "21", Attributes: jsontext.Value(`{}`), LastChanged: "2026-01-01T00:00:00Z", LastUpdated: "2026-01-01T00:00:00Z"},
	})

	lights, err := tr.GetEntities(ctx, "light.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(lights) != 2 {
		t.Errorf("expected 2 lights, got %d", len(lights))
	}

	ids, err := tr.GetEntityIDs(ctx, "light.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 entity IDs, got %d", len(ids))
	}
	// Sorted output: map iteration is random, callers (and tests) deserve
	// deterministic order.
	if ids[0] != "light.bedroom" || ids[1] != "light.kitchen" {
		t.Errorf("ids not sorted: %v", ids)
	}

	// A malformed glob is an error, not a silent empty result.
	if _, err := tr.GetEntities(ctx, "light.["); err == nil {
		t.Error("malformed pattern: expected an error")
	}
}

func TestMigrateIdempotent(t *testing.T) {
	writeDB, _ := testutil.NewTestDB(t, nil)
	if err := Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	// Second apply must not error
	if err := Migrate(writeDB); err != nil {
		t.Errorf("second migrate: %v", err)
	}
}

func BenchmarkStateInsert(b *testing.B) {
	writeDB, readDB := testutil.NewTestDB(b, nil)
	if err := Migrate(writeDB); err != nil {
		b.Fatal(err)
	}
	tr := New(writeDB, readDB)
	ctx := context.Background()
	tr.Start(ctx)
	_ = tr.Seed(ctx, nil) // ensure tables exist

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tr.HandleStateChanged(ctx, jsontext.Value(`{
			"entity_id":"bench.entity","new_state":{"entity_id":"bench.entity","state":"on","attributes":{},"last_changed":"2026-01-01T00:00:00Z","last_updated":"2026-01-01T00:00:00Z"}
		}`))
	}
	// Include the persistence drain so the number stays honest end-to-end.
	tr.Flush()
}
