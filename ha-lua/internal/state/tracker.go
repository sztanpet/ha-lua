package state

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/sztanpet/ha-lua/internal/ha"
)

// Tracker mirrors current entity state in memory and persists everything to
// SQLite. The memory map is authoritative for reads: it is updated before
// events are dispatched to scripts, so a handler's GetState always reflects
// every change dispatched before its own event — without putting SQLite on
// the read path. History queries stay on SQLite (that is what it is for).
//
// Persistence is write-behind: HandleStateChanged enqueues, and a single
// writer goroutine (Start) drains the queue into one batched transaction per
// wakeup. The event-dispatch path therefore never waits on the write handle —
// not for a WAL checkpoint, not for the purge DELETE, not for a script's
// store.set.
type Tracker struct {
	writeDB *sql.DB
	readDB  *sql.DB

	mu  sync.RWMutex
	mem map[string]ha.StateData

	queue chan writeReq
}

// writeReq is one unit of write-behind work: a history append (upsert !=
// nil) or a flush marker (flush != nil, closed by the writer once everything
// enqueued before it is committed).
type writeReq struct {
	upsert *ha.StateData
	flush  chan struct{}
}

// writeQueueCap bounds the write-behind queue. With batching, the writer
// outruns any realistic event rate; a full queue means the disk has stalled
// for thousands of events, and blocking (backpressure) is then honest —
// dropping history rows silently is not.
const writeQueueCap = 1024

// New creates a Tracker. Both handles must already have the schema applied.
// The memory mirror starts empty; Seed fills it (scripts only start after the
// first seed, so nothing reads before then). Call Start to begin persisting —
// until then writes only accumulate in the queue.
func New(writeDB, readDB *sql.DB) *Tracker {
	return &Tracker{
		writeDB: writeDB,
		readDB:  readDB,
		mem:     make(map[string]ha.StateData),
		queue:   make(chan writeReq, writeQueueCap),
	}
}

// Start launches the write-behind goroutine. It exits when ctx is cancelled,
// after a best-effort drain of whatever is already queued.
func (t *Tracker) Start(ctx context.Context) {
	go t.writeLoop(ctx)
}

// Flush blocks until every write enqueued before it is committed. Test
// helper by design; production code never needs a barrier (memory is
// authoritative and history is read on human timescales).
func (t *Tracker) Flush() {
	done := make(chan struct{})
	t.queue <- writeReq{flush: done}
	<-done
}

func (t *Tracker) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Best-effort final drain: commit what is buffered, then stop.
			// Anything arriving after this is lost, which is inside the
			// accepted durability class (same as a power cut).
			t.commitBatch(t.drainQueue(nil))
			return
		case req := <-t.queue:
			t.commitBatch(t.drainQueue([]writeReq{req}))
		}
	}
}

// drainQueue appends everything immediately available to batch.
func (t *Tracker) drainQueue(batch []writeReq) []writeReq {
	for {
		select {
		case req := <-t.queue:
			batch = append(batch, req)
		default:
			return batch
		}
	}
}

// commitBatch writes one batch in a single transaction, retrying once. On
// repeated failure the batch is dropped loudly: memory stays authoritative,
// scripts keep working, only history has a gap. Flush markers are resolved
// after the commit (or the drop — a barrier must not deadlock on a bad disk).
func (t *Tracker) commitBatch(batch []writeReq) {
	work := 0
	for _, req := range batch {
		if req.flush == nil {
			work++
		}
	}
	if work > 0 {
		err := t.writeBatch(batch)
		if err != nil {
			slog.Warn("state: batch write failed, retrying", "n", work, "err", err)
			err = t.writeBatch(batch)
		}
		if err != nil {
			slog.Error("state: batch write failed twice, dropping",
				"n", work, "err", err)
		}
	}
	for _, req := range batch {
		if req.flush != nil {
			close(req.flush)
		}
	}
}

func (t *Tracker) writeBatch(batch []writeReq) error {
	// context.Background on purpose: a daemon shutdown must not abort a
	// commit that is already in flight.
	ctx := context.Background()
	tx, err := t.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, req := range batch {
		if req.upsert == nil {
			continue
		}
		s := req.upsert
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO state_history(entity_id, state, attributes, changed_at)
			VALUES(?,?,?,?)`,
			s.EntityID, s.State, string(s.Attributes), s.LastChanged); err != nil {
			return fmt.Errorf("insert state_history %s: %w", s.EntityID, err)
		}
	}
	return tx.Commit()
}

// enqueue hands a write to the writer goroutine. Blocks when the queue is
// full (see writeQueueCap); ctx aborts the wait so a shutdown can't wedge
// the caller.
func (t *Tracker) enqueue(ctx context.Context, req writeReq) {
	select {
	case t.queue <- req:
	default:
		slog.Warn("state: write queue full, backpressuring event dispatch")
		select {
		case t.queue <- req:
		case <-ctx.Done():
			slog.Warn("state: shutdown with full write queue, dropping write")
		}
	}
}

// Seed replaces the memory mirror with the batch and appends a history row
// for every entity whose state or attributes differ from the last known
// value. The comparison baseline is the memory mirror when populated (a
// reconnect re-seed — it reflects every event applied so far), or, on a cold
// start, the newest history row per entity. Unconditional appends would fill
// the history with phantom state changes on every reconnect.
//
// Cold-start corollary: an entity whose entire history has been purged (it
// last changed before the retention window) gets one fresh baseline row per
// daemon restart. That is a truthful "state at startup" observation, bounded
// to one row per stale entity per restart, and it keeps the retention window
// self-contained.
//
// Ghost entities (present before, absent from the batch) drop out with the
// map replacement — the seed is the only place a removal that happened while
// disconnected can be observed. Their history ages out via the purge. An
// empty batch replaces nothing: get_states never legitimately returns zero
// states, and treating it as "everything was removed" would wipe the mirror
// on a protocol hiccup.
func (t *Tracker) Seed(ctx context.Context, states []ha.StateData) error {
	if len(states) == 0 {
		return nil
	}

	type mirror struct{ state, attrs string }
	current := make(map[string]mirror)
	t.mu.RLock()
	for id, s := range t.mem {
		current[id] = mirror{state: s.State, attrs: string(s.Attributes)}
	}
	t.mu.RUnlock()

	if len(current) == 0 {
		// Cold start: the newest history row per entity is the last state
		// this daemon ever recorded (id is the autoincrement insert order).
		rows, err := t.readDB.QueryContext(ctx, `
			SELECT entity_id, state, attributes FROM state_history
			WHERE id IN (SELECT MAX(id) FROM state_history GROUP BY entity_id)`)
		if err != nil {
			return fmt.Errorf("read history baseline: %w", err)
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
	}

	// Appends ride the write-behind queue like every other history row:
	// writeBatch is then the only place SQL is written, and the queue's FIFO
	// keeps insert order (and so the MAX(id) baseline above) total.
	mem := make(map[string]ha.StateData, len(states))
	for _, s := range states {
		s.Attributes = jsontext.Value(attrStr(s.Attributes))
		mem[s.EntityID] = s
		if m, ok := current[s.EntityID]; ok && m.state == s.State && m.attrs == string(s.Attributes) {
			continue
		}
		row := s
		t.enqueue(ctx, writeReq{upsert: &row})
	}
	t.mu.Lock()
	t.mem = mem
	t.mu.Unlock()
	return nil
}

// StateChangedData is the data portion of a state_changed event.
type StateChangedData struct {
	EntityID string        `json:"entity_id"`
	NewState *ha.StateData `json:"new_state"`
	OldState *ha.StateData `json:"old_state"`
}

// HandleStateChanged applies the change to the memory mirror synchronously
// (the caller dispatches to scripts right after, and their GetState must see
// it), then enqueues the SQLite work for the write-behind goroutine. Only a
// decode failure is an error; persistence problems are the writer's to log.
func (t *Tracker) HandleStateChanged(ctx context.Context, raw jsontext.Value) error {
	var data StateChangedData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode state_changed data: %w", err)
	}
	if data.NewState == nil {
		// HA sends a nil new_state when an entity is removed (e.g. deleting an
		// automation). That is normal lifecycle, not an error, so log it at
		// debug rather than warning. Drop the entity from the memory mirror so
		// get_state reports it as gone; state_history is append-only and keeps
		// the past states it really had. Nothing to persist — the mirror lives
		// in memory only.
		slog.Debug("state: entity removed (nil new_state)", "entity", data.EntityID)
		t.mu.Lock()
		delete(t.mem, data.EntityID)
		t.mu.Unlock()
		return nil
	}

	ns := *data.NewState
	ns.Attributes = jsontext.Value(attrStr(ns.Attributes))
	t.mu.Lock()
	t.mem[ns.EntityID] = ns
	t.mu.Unlock()
	t.enqueue(ctx, writeReq{upsert: &ns})
	return nil
}

// GetState returns the current state for an entity, from memory.
func (t *Tracker) GetState(_ context.Context, entityID string) (*ha.StateData, error) {
	t.mu.RLock()
	s, ok := t.mem[entityID]
	t.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	return &s, nil
}

// GetEntities returns all states whose entity_id matches the glob pattern
// (filepath.Match syntax — the same matcher ha.on_state_change patterns use),
// sorted by entity_id. Errors only on a malformed pattern.
func (t *Tracker) GetEntities(_ context.Context, pattern string) ([]ha.StateData, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var result []ha.StateData
	for id, s := range t.mem {
		ok, err := filepath.Match(pattern, id)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", pattern, err)
		}
		if ok {
			result = append(result, s)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].EntityID < result[j].EntityID })
	return result, nil
}

// GetEntityIDs returns entity IDs matching the glob pattern, sorted.
func (t *Tracker) GetEntityIDs(ctx context.Context, pattern string) ([]string, error) {
	states, err := t.GetEntities(ctx, pattern)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(states))
	for i := range states {
		ids[i] = states[i].EntityID
	}
	return ids, nil
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
