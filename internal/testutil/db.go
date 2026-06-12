// Package testutil provides shared test helpers: in-memory SQLite
// databases with the schema applied.
package testutil

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// NewTestDB opens an in-memory SQLite database with WAL mode and runs
// the provided migration function. The database is closed when t ends.
func NewTestDB(t testing.TB, migrate func(writeDB, readDB *sql.DB) error) (writeDB, readDB *sql.DB) {
	t.Helper()

	// Use a named shared-cache in-memory DB so the two handles share data.
	const dsn = "file::memory:?cache=shared&_journal_mode=WAL&_foreign_keys=on"

	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open write db: %v", err)
	}
	writeDB.SetMaxOpenConns(1)
	t.Cleanup(func() { writeDB.Close() })

	readDB, err = sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open read db: %v", err)
	}
	t.Cleanup(func() { readDB.Close() })

	if migrate != nil {
		if err := migrate(writeDB, readDB); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return writeDB, readDB
}
