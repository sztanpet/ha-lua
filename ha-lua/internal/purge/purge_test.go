package purge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

func newPurgeDB(tb testing.TB) *sql.DB {
	tb.Helper()
	writeDB, _ := testutil.NewTestDB(tb, nil)
	if err := state.Migrate(writeDB); err != nil {
		tb.Fatal(err)
	}
	return writeDB
}

func insertHistory(tb testing.TB, db *sql.DB, entityID string, changedAt time.Time) {
	tb.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO state_history(entity_id, state, attributes, changed_at)
		VALUES(?,?,?,?)`,
		entityID, "on", "{}", changedAt.UTC().Format(time.RFC3339))
	if err != nil {
		tb.Fatal(err)
	}
}

func countHistory(tb testing.TB, db *sql.DB) int {
	tb.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM state_history`).Scan(&n); err != nil {
		tb.Fatal(err)
	}
	return n
}

func TestRunOnce(t *testing.T) {
	db := newPurgeDB(t)
	now := time.Now().UTC()

	tests := []struct {
		name string
		age  time.Duration
		keep bool
	}{
		{"3 days old", 72 * time.Hour, false},
		{"just past retention", 49 * time.Hour, false},
		{"within window", 47 * time.Hour, true},
		{"1 hour old", time.Hour, true},
	}
	for i, tt := range tests {
		insertHistory(t, db, fmt.Sprintf("light.e%d", i), now.Add(-tt.age))
	}

	p := New(db, 2, time.Hour)
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	for i, tt := range tests {
		var n int
		err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM state_history WHERE entity_id = ?`,
			fmt.Sprintf("light.e%d", i)).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if kept := n == 1; kept != tt.keep {
			t.Errorf("%s: kept=%v, want keep=%v", tt.name, kept, tt.keep)
		}
	}
}

// With retention 0 days the cutoff is "now", so a row from one hour ago
// is on the same calendar day but older than the cutoff. Building the
// cutoff with datetime('now') in SQL would keep this row (space vs 'T'
// separator breaks the string comparison); the Go-side RFC3339 cutoff
// must delete it.
func TestRunOnceSameDayCutoff(t *testing.T) {
	db := newPurgeDB(t)
	insertHistory(t, db, "light.sameday", time.Now().UTC().Add(-time.Hour))

	p := New(db, 0, time.Hour)
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countHistory(t, db); n != 0 {
		t.Errorf("same-day expired row survived purge: %d rows left", n)
	}
}

func TestRunOnceEmptyTable(t *testing.T) {
	db := newPurgeDB(t)
	p := New(db, 2, time.Hour)
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkPurge(b *testing.B) {
	slog.SetLogLoggerLevel(slog.LevelError)
	for b.Loop() {
		b.StopTimer()
		db := newPurgeDB(b)
		old := time.Now().UTC().Add(-72 * time.Hour)
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			b.Fatal(err)
		}
		for i := range 10000 {
			if _, err := tx.ExecContext(context.Background(), `
				INSERT INTO state_history(entity_id, state, attributes, changed_at)
				VALUES(?,?,?,?)`,
				fmt.Sprintf("light.e%d", i), "on", "{}", old.Format(time.RFC3339)); err != nil {
				b.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
		p := New(db, 2, time.Hour)
		b.StartTimer()

		if err := p.RunOnce(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
