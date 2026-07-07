// Package state mirrors Home Assistant entity state into SQLite: a
// current-state table for fast lookups plus an append-only history log.
package state

import (
	"context"
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
CREATE INDEX IF NOT EXISTS idx_sh_time ON state_history(changed_at);

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
//
// synchronous=NORMAL because every state_changed event commits on the write
// handle BEFORE it is dispatched to scripts: at the default FULL, that is an
// fsync per event, and on the flash storage of a typical HA box fsync jitter
// (ms to tens of ms) shows up directly as handler-latency variance. NORMAL
// under WAL syncs only at checkpoints; a power loss can drop the last few
// commits but cannot corrupt the DB. Nothing here needs those commits: the
// states mirror is re-seeded from HA on every connect, history is short-lived
// observability data, and every/at timer rows are rebuilt at script load.
//
// modernc.org/sqlite takes pragmas as _pragma=name(value) — the mattn-style
// _journal_mode=WAL is silently ignored and leaves the default rollback
// journal. TestOpenDBEnablesWAL guards against regressing this.
func OpenDB(path string) (writeDB, readDB *sql.DB, err error) {
	pragmas := "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"

	// _txlock=immediate: write transactions take the write lock up front
	// instead of upgrading from a read lock mid-transaction, which is the
	// one place WAL can still return SQLITE_BUSY without honoring
	// busy_timeout.
	writeDB, err = sql.Open("sqlite", "file:"+path+"?"+pragmas+"&_txlock=immediate")
	if err != nil {
		return nil, nil, fmt.Errorf("open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)

	readDB, err = sql.Open("sqlite", "file:"+path+"?"+pragmas)
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
	_, err := db.ExecContext(context.Background(), schema)
	return err
}
