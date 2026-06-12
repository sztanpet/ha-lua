package state

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS states (
    entity_id    TEXT PRIMARY KEY,
    state        TEXT NOT NULL,
    attributes   TEXT NOT NULL DEFAULT '{}',
    last_changed TEXT NOT NULL,
    last_updated TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS state_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id  TEXT NOT NULL,
    state      TEXT NOT NULL,
    attributes TEXT NOT NULL DEFAULT '{}',
    changed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sh_entity_time ON state_history(entity_id, changed_at);

CREATE TABLE IF NOT EXISTS script_kv (
    script_id TEXT NOT NULL,
    key       TEXT NOT NULL,
    value     TEXT NOT NULL,
    PRIMARY KEY (script_id, key)
);

CREATE TABLE IF NOT EXISTS global_kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS timers (
    id        TEXT NOT NULL PRIMARY KEY,
    script_id TEXT NOT NULL,
    type      TEXT NOT NULL,
    spec      TEXT NOT NULL,
    last_run  TEXT,
    next_run  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_timers_next ON timers(next_run);
`

// OpenDB opens two handles against path: a single-connection write handle
// and a pooled read handle. WAL mode is enabled on open.
func OpenDB(path string) (writeDB, readDB *sql.DB, err error) {
	dsn := "file:" + path + "?_journal_mode=WAL&_foreign_keys=on"

	writeDB, err = sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)

	readDB, err = sql.Open("sqlite", dsn)
	if err != nil {
		writeDB.Close()
		return nil, nil, fmt.Errorf("open read db: %w", err)
	}

	if err = Migrate(writeDB); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	return writeDB, readDB, nil
}

// Migrate applies the schema to db. Safe to call multiple times (idempotent).
func Migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}
