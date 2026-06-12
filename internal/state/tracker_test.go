package state

import (
	"context"
	"testing"

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

	history, err := tr.GetHistory(ctx, "light.living", "2026-01-01T00:00:00Z", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) < 2 {
		t.Errorf("expected at least 2 history rows, got %d", len(history))
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
