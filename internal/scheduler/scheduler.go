// Package scheduler is the SQLite-backed timer engine behind ha.every,
// ha.at, and ha.after. Registrations live in the timers table; an
// in-memory min-heap picks the next deadline; fires are routed to script
// channels through an onFire callback, so the scheduler never holds a
// reference into the Lua layer.
//
// Catch-up needs no special startup phase: scripts re-register their
// timers on every load, INSERT OR IGNORE preserves the stored next_run,
// and a next_run that passed during downtime simply fires as soon as the
// loop sees it — once, because rescheduling computes from now.
package scheduler

import (
	"container/heap"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	// ha.at needs IANA zones inside a container that has no
	// /usr/share/zoneinfo.
	_ "time/tzdata"
)

// timer is one heap entry.
type timer struct {
	id       string
	scriptID string
	typ      string // "every" | "at" | "after"
	spec     string
	nextRun  time.Time
}

type timerHeap []*timer

func (h timerHeap) Len() int           { return len(h) }
func (h timerHeap) Less(i, j int) bool { return h[i].nextRun.Before(h[j].nextRun) }
func (h timerHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)        { *h = append(*h, x.(*timer)) }
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return t
}

// Scheduler owns the timers table and the min-heap. All heap access goes
// through mu; all DB access uses the single-connection write handle.
type Scheduler struct {
	db     *sql.DB
	loc    *time.Location
	onFire func(scriptID, timerID string)

	mu   sync.Mutex
	heap timerHeap
	wake chan struct{}
}

// New creates a Scheduler. db must be the write handle; loc is the
// wall-clock location for ha.at (see ResolveLocation); onFire is called
// from the scheduler goroutine for every fired timer and must not block.
func New(db *sql.DB, loc *time.Location, onFire func(scriptID, timerID string)) *Scheduler {
	return &Scheduler{
		db:     db,
		loc:    loc,
		onFire: onFire,
		wake:   make(chan struct{}, 1),
	}
}

// ResolveLocation picks the wall-clock location for ha.at: the timezone
// config option if set (invalid is a hard error — bad config should be
// loud), else $TZ (invalid falls back), else UTC with a warning.
func ResolveLocation(configured string) (*time.Location, error) {
	if configured != "" {
		loc, err := time.LoadLocation(configured)
		if err != nil {
			return nil, fmt.Errorf("bad timezone option %q: %w", configured, err)
		}
		return loc, nil
	}
	if tz := os.Getenv("TZ"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err == nil {
			return loc, nil
		}
		slog.Warn("scheduler: bad $TZ, ha.at timers will fire in UTC", "tz", tz, "err", err)
		return time.UTC, nil
	}
	slog.Warn("scheduler: no timezone configured, ha.at timers will fire in UTC")
	return time.UTC, nil
}

// Start deletes orphaned ha.after rows and spawns the timer loop. Must be
// called before any script loads: every "after" row found here is a
// leftover from the previous process whose callback is unrecoverable.
func (s *Scheduler) Start(ctx context.Context) error {
	if err := s.cleanOrphans(ctx); err != nil {
		return fmt.Errorf("clean orphaned timers: %w", err)
	}
	go s.loop(ctx)
	return nil
}

func (s *Scheduler) cleanOrphans(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, script_id FROM timers WHERE type = 'after'`)
	if err != nil {
		return err
	}
	// Drain fully before the DELETE: the write handle has one connection,
	// and an open result set holds it.
	var orphans [][2]string
	for rows.Next() {
		var id, scriptID string
		if err := rows.Scan(&id, &scriptID); err != nil {
			rows.Close()
			return err
		}
		orphans = append(orphans, [2]string{id, scriptID})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(orphans) == 0 {
		return nil
	}
	for _, o := range orphans {
		slog.Warn("scheduler: dropping ha.after timer from previous run, callback is unrecoverable",
			"timer", o[0], "script", o[1])
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM timers WHERE type = 'after'`)
	return err
}

// RegisterEvery registers a recurring interval timer. The ID is stable
// across reloads, so INSERT OR IGNORE keeps the stored next_run and a
// deadline missed during downtime fires as soon as the loop runs.
func (s *Scheduler) RegisterEvery(ctx context.Context, scriptID, spec string, seq int) (string, error) {
	d, err := parseInterval(spec)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s|every|%s|%d", scriptID, spec, seq)
	return id, s.insert(ctx, id, scriptID, "every", spec, time.Now().UTC().Add(d))
}

// RegisterAt registers a daily wall-clock timer ("HH:MM" in the resolved
// location). Stable-ID semantics are the same as RegisterEvery.
func (s *Scheduler) RegisterAt(ctx context.Context, scriptID, spec string, seq int) (string, error) {
	next, err := nextAt(spec, time.Now(), s.loc)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s|at|%s|%d", scriptID, spec, seq)
	return id, s.insert(ctx, id, scriptID, "at", spec, next)
}

// RegisterAfter registers a one-shot timer. The ID is random — concurrent
// calls with the same delay are distinct timers — which is also why the
// row never survives a restart (see Start).
func (s *Scheduler) RegisterAfter(ctx context.Context, scriptID, spec string) (string, error) {
	d, err := parseInterval(spec)
	if err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s|after|%s", scriptID, hex.EncodeToString(b[:]))
	return id, s.insert(ctx, id, scriptID, "after", spec, time.Now().UTC().Add(d))
}

// PruneScript deletes timer rows for scriptID that are not in keep —
// leftovers from ha.every/ha.at calls removed from the script source.
// The runner calls this once the script has finished loading, so keep is
// exactly the set of IDs just registered; pruned rows are by definition
// not in the heap.
func (s *Scheduler) PruneScript(ctx context.Context, scriptID string, keep []string) error {
	args := make([]any, 0, len(keep)+1)
	args = append(args, scriptID)
	for _, id := range keep {
		args = append(args, id)
	}
	q := `DELETE FROM timers WHERE script_id = ?`
	if len(keep) > 0 {
		q += ` AND id NOT IN (?` + strings.Repeat(",?", len(keep)-1) + `)`
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// RemoveScript drops the script's timers from the heap and deletes its
// ha.after rows. every/at rows stay: their stable IDs are what carries
// last_run/next_run across the reload that usually follows a stop.
func (s *Scheduler) RemoveScript(scriptID string) {
	s.mu.Lock()
	kept := s.heap[:0]
	for _, t := range s.heap {
		if t.scriptID != scriptID {
			kept = append(kept, t)
		}
	}
	for i := len(kept); i < len(s.heap); i++ {
		s.heap[i] = nil
	}
	s.heap = kept
	heap.Init(&s.heap)
	s.mu.Unlock()

	if _, err := s.db.ExecContext(context.Background(),
		`DELETE FROM timers WHERE script_id = ? AND type = 'after'`, scriptID); err != nil {
		slog.Warn("scheduler: after-timer cleanup failed", "script", scriptID, "err", err)
	}
}

// insert writes the row — INSERT OR IGNORE, an existing row wins — and
// pushes whatever next_run actually ended up stored onto the heap. For a
// pre-existing row that stored value may be in the past: that is the
// catch-up case, and the loop fires it immediately.
func (s *Scheduler) insert(ctx context.Context, id, scriptID, typ, spec string, next time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO timers (id, script_id, type, spec, next_run) VALUES (?, ?, ?, ?, ?)`,
		id, scriptID, typ, spec, next.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert timer %s: %w", id, err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx,
		`SELECT next_run FROM timers WHERE id = ?`, id).Scan(&stored); err != nil {
		return fmt.Errorf("read back timer %s: %w", id, err)
	}
	nr, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		return fmt.Errorf("timer %s has bad next_run %q: %w", id, stored, err)
	}

	s.mu.Lock()
	heap.Push(&s.heap, &timer{id: id, scriptID: scriptID, typ: typ, spec: spec, nextRun: nr})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *Scheduler) loop(ctx context.Context) {
	for {
		s.fireDue(ctx)

		s.mu.Lock()
		d := time.Hour
		if len(s.heap) > 0 {
			d = time.Until(s.heap[0].nextRun)
		}
		s.mu.Unlock()
		if d < 0 {
			d = 0
		}

		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(d):
		}
	}
}

// fireDue pops and fires every timer whose deadline has passed. It runs
// entirely under the heap lock: onFire is a non-blocking channel send and
// the DB writes are serialized by the single-connection handle anyway, so
// dropping the lock between pops would buy nothing.
func (s *Scheduler) fireDue(ctx context.Context) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.heap) > 0 && !s.heap[0].nextRun.After(now) {
		t := heap.Pop(&s.heap).(*timer)
		s.fire(ctx, t, now)
	}
}

// fire updates the row and reschedules recurring timers. Recompute is
// always from now, never from the stored next_run: after downtime a
// missed timer fires exactly once instead of replaying every missed
// interval. Caller holds s.mu.
func (s *Scheduler) fire(ctx context.Context, t *timer, now time.Time) {
	switch t.typ {
	case "after":
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM timers WHERE id = ?`, t.id); err != nil {
			slog.Warn("scheduler: timer row delete failed", "timer", t.id, "err", err)
		}
	default:
		var next time.Time
		var err error
		if t.typ == "every" {
			var d time.Duration
			d, err = parseInterval(t.spec)
			next = now.Add(d)
		} else {
			next, err = nextAt(t.spec, now, s.loc)
		}
		if err != nil {
			// Registration validated the spec, so this is a corrupted row.
			// Don't reschedule garbage.
			slog.Error("scheduler: bad spec on fire, dropping timer", "timer", t.id, "err", err)
			return
		}
		t.nextRun = next
		if _, err := s.db.ExecContext(ctx,
			`UPDATE timers SET last_run = ?, next_run = ? WHERE id = ?`,
			now.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano), t.id); err != nil {
			slog.Warn("scheduler: timer row update failed", "timer", t.id, "err", err)
		}
		heap.Push(&s.heap, t)
	}
	s.onFire(t.scriptID, t.id)
}

func parseInterval(spec string) (time.Duration, error) {
	d, err := time.ParseDuration(spec)
	if err != nil {
		return 0, fmt.Errorf("bad interval %q: %w", spec, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("interval %q must be positive", spec)
	}
	return d, nil
}

// nextAt maps "HH:MM" to the next wall-clock occurrence in loc, returned
// in UTC — next_run is always stored UTC; loc only decides what instant
// the wall-clock time means.
func nextAt(spec string, now time.Time, loc *time.Location) (time.Time, error) {
	hm, err := time.Parse("15:04", spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad time %q (want HH:MM): %w", spec, err)
	}
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if !next.After(n) {
		next = next.AddDate(0, 0, 1)
	}
	return next.UTC(), nil
}
