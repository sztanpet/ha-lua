package state

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/sztanpet/ha-lua/internal/ha"
)

// Tracker writes state changes to SQLite and serves read queries from
// the read handle.
type Tracker struct {
	writeDB *sql.DB
	readDB  *sql.DB
}

// New creates a Tracker. Both handles must already have the schema applied.
func New(writeDB, readDB *sql.DB) *Tracker {
	return &Tracker{writeDB: writeDB, readDB: readDB}
}

// Seed upserts all states returned by get_states into the mirror. A history
// row is appended only when the state or attributes differ from the mirror —
// reconnects re-seed, and unconditional appends would fill the history with
// phantom state changes.
func (t *Tracker) Seed(ctx context.Context, states []ha.StateData) error {
	tx, err := t.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type mirror struct{ state, attrs string }
	current := make(map[string]mirror)
	rows, err := tx.QueryContext(ctx, `SELECT entity_id, state, attributes FROM states`)
	if err != nil {
		return fmt.Errorf("read mirror: %w", err)
	}
	for rows.Next() {
		var id string
		var m mirror
		if err := rows.Scan(&id, &m.state, &m.attrs); err != nil {
			rows.Close()
			return err
		}
		current[id] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, s := range states {
		attrs := attrStr(s.Attributes)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO states(entity_id, state, attributes, last_changed, last_updated)
			VALUES(?,?,?,?,?)
			ON CONFLICT(entity_id) DO UPDATE SET
			  state=excluded.state, attributes=excluded.attributes,
			  last_changed=excluded.last_changed, last_updated=excluded.last_updated`,
			s.EntityID, s.State, attrs, s.LastChanged, s.LastUpdated); err != nil {
			return fmt.Errorf("upsert states %s: %w", s.EntityID, err)
		}
		if m, ok := current[s.EntityID]; ok && m.state == s.State && m.attrs == attrs {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO state_history(entity_id, state, attributes, changed_at)
			VALUES(?,?,?,?)`,
			s.EntityID, s.State, attrs, s.LastChanged); err != nil {
			return fmt.Errorf("insert state_history %s: %w", s.EntityID, err)
		}
	}
	return tx.Commit()
}

// StateChangedData is the data portion of a state_changed event.
type StateChangedData struct {
	EntityID string        `json:"entity_id"`
	NewState *ha.StateData `json:"new_state"`
	OldState *ha.StateData `json:"old_state"`
}

// HandleStateChanged upserts the new state and appends a history row.
func (t *Tracker) HandleStateChanged(ctx context.Context, raw jsontext.Value) error {
	var data StateChangedData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode state_changed data: %w", err)
	}
	if data.NewState == nil {
		// HA sends a nil new_state when an entity is removed (e.g. deleting an
		// automation). That is normal lifecycle, not an error, so log it at
		// debug rather than warning.
		slog.Debug("state: entity removed (nil new_state)", "entity", data.EntityID)
		return nil
	}

	ns := data.NewState
	attrs := attrStr(ns.Attributes)
	tx, err := t.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO states(entity_id, state, attributes, last_changed, last_updated)
		VALUES(?,?,?,?,?)
		ON CONFLICT(entity_id) DO UPDATE SET
		  state=excluded.state, attributes=excluded.attributes,
		  last_changed=excluded.last_changed, last_updated=excluded.last_updated`,
		ns.EntityID, ns.State, attrs, ns.LastChanged, ns.LastUpdated); err != nil {
		return fmt.Errorf("upsert states %s: %w", ns.EntityID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO state_history(entity_id, state, attributes, changed_at)
		VALUES(?,?,?,?)`,
		ns.EntityID, ns.State, attrs, ns.LastChanged); err != nil {
		return fmt.Errorf("insert state_history %s: %w", ns.EntityID, err)
	}
	return tx.Commit()
}

// GetState returns the current state for an entity.
func (t *Tracker) GetState(ctx context.Context, entityID string) (*ha.StateData, error) {
	row := t.readDB.QueryRowContext(ctx,
		`SELECT entity_id, state, attributes, last_changed, last_updated
		 FROM states WHERE entity_id = ?`, entityID)
	var s ha.StateData
	var attrs string
	if err := row.Scan(&s.EntityID, &s.State, &attrs, &s.LastChanged, &s.LastUpdated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	s.Attributes = jsontext.Value(attrs)
	return &s, nil
}

// GetEntities returns all states whose entity_id matches the SQL GLOB pattern.
func (t *Tracker) GetEntities(ctx context.Context, pattern string) ([]ha.StateData, error) {
	rows, err := t.readDB.QueryContext(ctx,
		`SELECT entity_id, state, attributes, last_changed, last_updated
		 FROM states WHERE entity_id GLOB ?`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStates(rows)
}

// GetEntityIDs returns entity IDs matching the glob pattern.
func (t *Tracker) GetEntityIDs(ctx context.Context, pattern string) ([]string, error) {
	rows, err := t.readDB.QueryContext(ctx,
		`SELECT entity_id FROM states WHERE entity_id GLOB ?`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// sinceLayout renders the get_history `since` bound. changed_at holds HA's
// last_changed verbatim — an ISO8601 UTC instant like
// "2026-06-21T10:30:00.123456+00:00" — and the WHERE clause compares it as a
// plain string. Rendering `since` in UTC, truncated to whole seconds and with
// no zone suffix, makes it a lexical prefix of any same-second changed_at, so
// "changed_at >= since" is correct for every caller timezone and HA precision.
// Callers pass a time.Time; they never hand-format a comparable string.
const sinceLayout = "2006-01-02T15:04:05"

// GetHistory returns state history for an entity since a given instant.
func (t *Tracker) GetHistory(ctx context.Context, entityID string, since time.Time, limit int) ([]ha.StateData, error) {
	rows, err := t.readDB.QueryContext(ctx,
		`SELECT entity_id, state, attributes, changed_at, changed_at
		 FROM state_history
		 WHERE entity_id = ? AND changed_at >= ?
		 ORDER BY changed_at
		 LIMIT ?`, entityID, since.UTC().Format(sinceLayout), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStates(rows)
}

func scanStates(rows *sql.Rows) ([]ha.StateData, error) {
	var result []ha.StateData
	for rows.Next() {
		var s ha.StateData
		var attrs string
		if err := rows.Scan(&s.EntityID, &s.State, &attrs, &s.LastChanged, &s.LastUpdated); err != nil {
			return nil, err
		}
		s.Attributes = jsontext.Value(attrs)
		result = append(result, s)
	}
	return result, rows.Err()
}

func attrStr(v jsontext.Value) string {
	if len(v) == 0 {
		return "{}"
	}
	return string(v)
}
