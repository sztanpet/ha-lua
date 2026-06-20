// Package testutil provides shared test helpers: in-memory SQLite
// databases with the schema applied.
package testutil

import (
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

var testDBCounter atomic.Int64

// NewTestDB opens an in-memory SQLite database and runs the provided
// migration function. The database is closed when t ends. WAL does not
// apply to in-memory databases — concurrency behavior against a real
// file is covered by state.TestOpenDBEnablesWAL.
func NewTestDB(t testing.TB, migrate func(writeDB, readDB *sql.DB) error) (writeDB, readDB *sql.DB) {
	t.Helper()

	// A named shared-cache in-memory DB lets the two handles see the same
	// data. The name must be unique per test: every connection in the
	// process that opens the same URI shares one database, so a fixed
	// name would leak state between parallel tests.
	dsn := fmt.Sprintf("file:testdb%d?mode=memory&cache=shared&_pragma=foreign_keys(on)",
		testDBCounter.Add(1))

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
