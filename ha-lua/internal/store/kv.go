// Package store provides per-script and global key-value storage over
// SQLite. Values round-trip as JSON so Lua types are preserved.
package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// table is the implementation both stores share. They differ only in which
// table they address and, for the per-script one, in the script_id every
// statement is scoped by.
type table struct {
	writeDB *sql.DB
	readDB  *sql.DB
	get     string
	set     string
	del     string
	all     string
	// scope prefixes every statement's bind arguments: the script id for
	// script_kv, nothing for global_kv.
	scope []any
}

func (t *table) args(rest ...any) []any {
	return append(append(make([]any, 0, len(t.scope)+len(rest)), t.scope...), rest...)
}

// Get returns the stored Go value for key, or nil if absent.
func (t *table) Get(ctx context.Context, key string) (any, error) {
	var raw string
	err := t.readDB.QueryRowContext(ctx, t.get, t.args(key)...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeValue(raw)
}

// Set persists value for key. Accepted types: nil, bool, float64, string, map,
// slice. A nil value deletes the key.
func (t *table) Set(ctx context.Context, key string, value any) error {
	if value == nil {
		return t.Delete(ctx, key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = t.writeDB.ExecContext(ctx, t.set, t.args(key, string(encoded))...)
	return err
}

// Delete removes key.
func (t *table) Delete(ctx context.Context, key string) error {
	_, err := t.writeDB.ExecContext(ctx, t.del, t.args(key)...)
	return err
}

// GetAll returns every key→value pair in scope.
func (t *table) GetAll(ctx context.Context) (map[string]any, error) {
	rows, err := t.readDB.QueryContext(ctx, t.all, t.scope...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]any)
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		decoded, err := decodeValue(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %q: %w", key, err)
		}
		result[key] = decoded
	}
	return result, rows.Err()
}

// Store is one script's private key-value storage.
type Store struct{ table }

// New returns a Store scoped to scriptID.
func New(writeDB, readDB *sql.DB, scriptID string) *Store {
	return &Store{table{
		writeDB: writeDB,
		readDB:  readDB,
		get:     `SELECT value FROM script_kv WHERE script_id = ? AND key = ?`,
		set: `INSERT INTO script_kv(script_id, key, value) VALUES(?,?,?)
		      ON CONFLICT(script_id, key) DO UPDATE SET value=excluded.value`,
		del:   `DELETE FROM script_kv WHERE script_id = ? AND key = ?`,
		all:   `SELECT key, value FROM script_kv WHERE script_id = ?`,
		scope: []any{scriptID},
	}}
}

// GlobalStore is the storage every script shares.
type GlobalStore struct{ table }

// NewGlobal returns a GlobalStore.
func NewGlobal(writeDB, readDB *sql.DB) *GlobalStore {
	return &GlobalStore{table{
		writeDB: writeDB,
		readDB:  readDB,
		get:     `SELECT value FROM global_kv WHERE key = ?`,
		set: `INSERT INTO global_kv(key, value) VALUES(?,?)
		      ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		del: `DELETE FROM global_kv WHERE key = ?`,
		all: `SELECT key, value FROM global_kv`,
	}}
}

func decodeValue(raw string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return v, nil
}
