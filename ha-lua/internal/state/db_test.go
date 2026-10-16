package state

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The whole two-handle design (single-connection writer, pooled readers)
// depends on WAL. modernc.org/sqlite ignores unknown DSN parameters, so a
// typo in the pragma syntax silently falls back to a rollback journal —
// which is exactly what happened once. Never again.
func TestOpenDBEnablesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	writeDB, readDB, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer writeDB.Close()
	defer readDB.Close()

	for name, db := range map[string]*sql.DB{"write": writeDB, "read": readDB} {
		var mode string
		if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("%s: query journal_mode: %v", name, err)
		}
		if mode != "wal" {
			t.Errorf("%s handle journal_mode = %q, want wal", name, mode)
		}
		var fk int
		if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("%s: query foreign_keys: %v", name, err)
		}
		if fk != 1 {
			t.Errorf("%s handle foreign_keys = %d, want 1", name, fk)
		}
		// 1 = NORMAL. FULL would put an fsync per state_changed commit on the
		// event-dispatch critical path.
		var sync int
		if err := db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatalf("%s: query synchronous: %v", name, err)
		}
		if sync != 1 {
			t.Errorf("%s handle synchronous = %d, want 1 (NORMAL)", name, sync)
		}
	}
}

func autoVacuum(t *testing.T, db *sql.DB) int {
	t.Helper()
	var mode int
	if err := db.QueryRowContext(t.Context(), "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

func TestOpenDBUsesIncrementalVacuum(t *testing.T) {
	writeDB, readDB, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writeDB.Close()
	defer readDB.Close()
	if mode := autoVacuum(t, readDB); mode != 2 {
		t.Errorf("auto_vacuum = %d, want 2 (incremental)", mode)
	}
}

// A database created before incremental vacuum keeps its data across the
// one-off rewrite that switches it.
func TestOpenDBConvertsOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE old_data(name TEXT PRIMARY KEY, value TEXT)`,
		`INSERT INTO old_data VALUES('mode', 'night')`,
	} {
		if _, err := old.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if mode := autoVacuum(t, old); mode != 0 {
		t.Fatalf("setup: auto_vacuum = %d, want 0", mode)
	}
	old.Close()

	writeDB, readDB, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writeDB.Close()
	defer readDB.Close()
	if mode := autoVacuum(t, readDB); mode != 2 {
		t.Errorf("auto_vacuum = %d after open, want 2 (incremental)", mode)
	}
	var value string
	if err := readDB.QueryRowContext(t.Context(),
		`SELECT value FROM old_data WHERE name = 'mode'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "night" {
		t.Errorf("value = %q after conversion, want night", value)
	}
}
