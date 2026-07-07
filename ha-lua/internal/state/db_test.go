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
