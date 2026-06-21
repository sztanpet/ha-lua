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
	return New(writeDB, readDB)
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
	_ = tr.Seed(context.Background(), nil) // ensure tables exist
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tr.HandleStateChanged(ctx, jsontext.Value(`{
			"entity_id":"bench.entity","new_state":{"entity_id":"bench.entity","state":"on","attributes":{},"last_changed":"2026-01-01T00:00:00Z","last_updated":"2026-01-01T00:00:00Z"}
		}`))
	}
}
