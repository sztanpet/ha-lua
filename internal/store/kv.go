package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-json-experiment/json"
)

// Store handles per-script key-value persistence over SQLite.
type Store struct {
	writeDB  *sql.DB
	readDB   *sql.DB
	scriptID string
}

// New returns a Store scoped to scriptID.
func New(writeDB, readDB *sql.DB, scriptID string) *Store {
	return &Store{writeDB: writeDB, readDB: readDB, scriptID: scriptID}
}

// Get returns the stored Go value for key, or nil if absent.
func (s *Store) Get(ctx context.Context, key string) (any, error) {
	row := s.readDB.QueryRowContext(ctx,
		`SELECT value FROM script_kv WHERE script_id = ? AND key = ?`,
		s.scriptID, key)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return decodeValue(raw)
}

// Set persists value for key. Accepted types: nil, bool, float64, string, map, slice.
func (s *Store) Set(ctx context.Context, key string, value any) error {
	if value == nil {
		return s.Delete(ctx, key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = s.writeDB.ExecContext(ctx,
		`INSERT INTO script_kv(script_id, key, value) VALUES(?,?,?)
		 ON CONFLICT(script_id, key) DO UPDATE SET value=excluded.value`,
		s.scriptID, key, string(encoded))
	return err
}

// Delete removes key.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.writeDB.ExecContext(ctx,
		`DELETE FROM script_kv WHERE script_id = ? AND key = ?`,
		s.scriptID, key)
	return err
}

// GetAll returns all key→value pairs for this script.
func (s *Store) GetAll(ctx context.Context) (map[string]any, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT key, value FROM script_kv WHERE script_id = ?`, s.scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]any)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		decoded, err := decodeValue(v)
		if err != nil {
			return nil, fmt.Errorf("decode %q: %w", k, err)
		}
		result[k] = decoded
	}
	return result, rows.Err()
}

// GlobalStore handles the shared global_kv table.
type GlobalStore struct {
	writeDB *sql.DB
	readDB  *sql.DB
}

// NewGlobal returns a GlobalStore.
func NewGlobal(writeDB, readDB *sql.DB) *GlobalStore {
	return &GlobalStore{writeDB: writeDB, readDB: readDB}
}

func (g *GlobalStore) Get(ctx context.Context, key string) (any, error) {
	row := g.readDB.QueryRowContext(ctx,
		`SELECT value FROM global_kv WHERE key = ?`, key)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return decodeValue(raw)
}

func (g *GlobalStore) Set(ctx context.Context, key string, value any) error {
	if value == nil {
		return g.Delete(ctx, key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = g.writeDB.ExecContext(ctx,
		`INSERT INTO global_kv(key, value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, string(encoded))
	return err
}

func (g *GlobalStore) Delete(ctx context.Context, key string) error {
	_, err := g.writeDB.ExecContext(ctx,
		`DELETE FROM global_kv WHERE key = ?`, key)
	return err
}

func (g *GlobalStore) GetAll(ctx context.Context) (map[string]any, error) {
	rows, err := g.readDB.QueryContext(ctx, `SELECT key, value FROM global_kv`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]any)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		decoded, err := decodeValue(v)
		if err != nil {
			return nil, fmt.Errorf("decode %q: %w", k, err)
		}
		result[k] = decoded
	}
	return result, rows.Err()
}

func decodeValue(raw string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return v, nil
}
