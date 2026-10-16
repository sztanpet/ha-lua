package purge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

func TestRunOnceKeepRules(t *testing.T) {
	db := newPurgeDB(t)
	old := time.Now().UTC().Add(-72 * time.Hour)

	insertHistory(t, db, "binary_sensor.front_door", old) // kept 30 days
	insertHistory(t, db, "binary_sensor.motion", old)     // kept 30 days
	insertHistory(t, db, "sensor.chatty", old)            // rule says 1 day
	insertHistory(t, db, "light.hall", old)               // default 2 days

	p := New(db, 2, time.Hour,
		Rule{Pattern: "binary_sensor.*", Days: 30},
		Rule{Pattern: "sensor.*", Days: 1},
	)
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	for entity, want := range map[string]bool{
		"binary_sensor.front_door": true,
		"binary_sensor.motion":     true,
		"sensor.chatty":            false,
		"light.hall":               false,
	} {
		var n int
		err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM state_history WHERE entity_id = ?`, entity).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if kept := n == 1; kept != want {
			t.Errorf("%s: kept=%v, want %v", entity, kept, want)
		}
	}
}

// Overlapping globs: the first rule wins, so a later, more aggressive rule
// cannot delete what an earlier one promised to keep.
func TestRunOnceFirstRuleWins(t *testing.T) {
	db := newPurgeDB(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	insertHistory(t, db, "sensor.boiler_runtime", old)

	p := New(db, 2, time.Hour,
		Rule{Pattern: "sensor.boiler_*", Days: 30},
		Rule{Pattern: "sensor.*", Days: 1},
	)
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countHistory(t, db); n != 1 {
		t.Errorf("the later sensor.* rule deleted a row sensor.boiler_* kept")
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

// fillHistory writes n rows of changedAt age with attributes big enough that
// the row count drives the page count.
func fillHistory(tb testing.TB, db *sql.DB, n int, changedAt time.Time) {
	tb.Helper()
	attrs := `{"pad":"` + strings.Repeat("x", 400) + `"}`
	tx, err := db.BeginTx(tb.Context(), nil)
	if err != nil {
		tb.Fatal(err)
	}
	for i := range n {
		if _, err := tx.ExecContext(tb.Context(), `
			INSERT INTO state_history(entity_id, state, attributes, changed_at)
			VALUES(?,?,?,?)`,
			fmt.Sprintf("sensor.s%d", i), "1", attrs, changedAt.UTC().Format(time.RFC3339)); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

func openFileDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ha-lua.db")
	writeDB, readDB, err := state.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writeDB.Close(); readDB.Close() })
	return writeDB, path
}

func TestRunOnceVacuumsMostlyFreeFile(t *testing.T) {
	db, path := openFileDB(t)
	now := time.Now()
	fillHistory(t, db, 3000, now.Add(-72*time.Hour))
	fillHistory(t, db, 300, now)
	pagesBefore, _, _, err := state.Pages(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}

	if err := New(db, 2, time.Hour).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	pages, free, _, err := state.Pages(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if free != 0 || pages*3 > pagesBefore {
		t.Errorf("after purge: %d pages (%d free), was %d; want vacuumed", pages, free, pagesBefore)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("WAL is %d bytes after vacuum, want truncated", fi.Size())
	}
	if got := countHistory(t, db); got != 300 {
		t.Errorf("%d rows survived, want 300", got)
	}
}

// Deletes the next inserts will reuse are not worth rewriting the file for.
func TestRunOnceLeavesSmallFreeListAlone(t *testing.T) {
	db, _ := openFileDB(t)
	now := time.Now()
	fillHistory(t, db, 300, now.Add(-72*time.Hour))
	fillHistory(t, db, 3000, now)

	if err := New(db, 2, time.Hour).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, free, _, err := state.Pages(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Error("free list is empty: a ~10% free file was vacuumed")
	}
}
