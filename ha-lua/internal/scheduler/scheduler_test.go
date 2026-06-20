package scheduler

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

type firing struct{ scriptID, timerID string }

func newTestSched(t testing.TB, loc *time.Location) (*Scheduler, chan firing, *sql.DB) {
	t.Helper()
	writeDB, _ := testutil.NewTestDB(t, func(w, _ *sql.DB) error { return state.Migrate(w) })
	fired := make(chan firing, 256)
	s := New(writeDB, loc, func(scriptID, timerID string) {
		fired <- firing{scriptID, timerID}
	})
	return s, fired, writeDB
}

func waitFire(t *testing.T, ch chan firing) firing {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not fire")
		return firing{}
	}
}

func assertNoFire(t *testing.T, ch chan firing, d time.Duration) {
	t.Helper()
	select {
	case f := <-ch:
		t.Fatalf("unexpected fire: %+v", f)
	case <-time.After(d):
	}
}

func timerCount(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM timers`
	if where != "" {
		q += ` WHERE ` + where
	}
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEveryFiresAndReschedules(t *testing.T) {
	t.Parallel()
	s, fired, db := newTestSched(t, time.UTC)
	ctx := t.Context()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}

	id, err := s.RegisterEvery(ctx, "s1", "30ms", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "s1|every|30ms|1"; id != want {
		t.Fatalf("id = %q, want %q", id, want)
	}

	// Two fires prove the timer reschedules, not just fires once.
	for i := 0; i < 2; i++ {
		f := waitFire(t, fired)
		if f.scriptID != "s1" || f.timerID != id {
			t.Fatalf("fire %d = %+v", i, f)
		}
	}

	var lastRun sql.NullString
	var nextRun string
	if err := db.QueryRowContext(ctx, `SELECT last_run, next_run FROM timers WHERE id = ?`, id).
		Scan(&lastRun, &nextRun); err != nil {
		t.Fatal(err)
	}
	if !lastRun.Valid {
		t.Error("last_run not set after fire")
	}
	nr, err := time.Parse(time.RFC3339Nano, nextRun)
	if err != nil {
		t.Fatalf("bad next_run %q: %v", nextRun, err)
	}
	if !nr.After(time.Now().UTC().Add(-time.Second)) {
		t.Errorf("next_run %v not rescheduled into the future", nr)
	}
}

func TestEveryDoesNotFireBeforeInterval(t *testing.T) {
	t.Parallel()
	s, fired, _ := newTestSched(t, time.UTC)
	ctx := t.Context()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterEvery(ctx, "s1", "1h", 1); err != nil {
		t.Fatal(err)
	}
	assertNoFire(t, fired, 100*time.Millisecond)
}

func TestCatchUpFiresOnce(t *testing.T) {
	t.Parallel()
	s, fired, db := newTestSched(t, time.UTC)
	ctx := t.Context()

	// A row from a "previous run" whose deadline passed during downtime.
	id := "s1|every|1h|1"
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO timers (id, script_id, type, spec, last_run, next_run) VALUES (?, 's1', 'every', '1h', ?, ?)`,
		id, past, past); err != nil {
		t.Fatal(err)
	}

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	gotID, err := s.RegisterEvery(ctx, "s1", "1h", 1)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != id {
		t.Fatalf("re-registration produced %q, want %q", gotID, id)
	}

	f := waitFire(t, fired)
	if f.timerID != id {
		t.Fatalf("fire = %+v", f)
	}
	// Fired once, not replayed per missed interval.
	assertNoFire(t, fired, 100*time.Millisecond)

	var nextRun string
	if err := db.QueryRowContext(ctx, `SELECT next_run FROM timers WHERE id = ?`, id).Scan(&nextRun); err != nil {
		t.Fatal(err)
	}
	nr, err := time.Parse(time.RFC3339Nano, nextRun)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(nr); until < 50*time.Minute || until > 70*time.Minute {
		t.Errorf("next_run %v not recomputed from now (until = %v)", nr, until)
	}
}

func TestNextAt(t *testing.T) {
	t.Parallel()
	bud, err := time.LoadLocation("Europe/Budapest")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-01-15 12:00 UTC = 13:00 CET (winter, UTC+1).
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		spec string
		loc  *time.Location
		want time.Time
	}{
		{"future today UTC", "18:30", time.UTC,
			time.Date(2026, 1, 15, 18, 30, 0, 0, time.UTC)},
		{"past today rolls to tomorrow", "07:00", time.UTC,
			time.Date(2026, 1, 16, 7, 0, 0, 0, time.UTC)},
		{"exactly now rolls to tomorrow", "12:00", time.UTC,
			time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC)},
		// 14:00 CET = 13:00 UTC, still ahead of 12:00 UTC.
		{"timezone offset applied", "14:00", bud,
			time.Date(2026, 1, 15, 13, 0, 0, 0, time.UTC)},
		// 13:00 CET = 12:00 UTC = now, so tomorrow.
		{"timezone now rolls to tomorrow", "13:00", bud,
			time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextAt(tt.spec, now, tt.loc)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("nextAt(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}

	for _, bad := range []string{"7am", "25:00", "07:60", "", "07:00:00"} {
		if _, err := nextAt(bad, now, time.UTC); err == nil {
			t.Errorf("nextAt(%q) accepted a bad spec", bad)
		}
	}
}

func TestAfterFiresOnceAndDeletesRow(t *testing.T) {
	t.Parallel()
	s, fired, db := newTestSched(t, time.UTC)
	ctx := t.Context()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}

	id, err := s.RegisterAfter(ctx, "s1", "20ms")
	if err != nil {
		t.Fatal(err)
	}
	if timerCount(t, db, "id = ?", id) != 1 {
		t.Fatal("row not written before fire")
	}

	f := waitFire(t, fired)
	if f.timerID != id {
		t.Fatalf("fire = %+v", f)
	}
	assertNoFire(t, fired, 100*time.Millisecond)
	if timerCount(t, db, "id = ?", id) != 0 {
		t.Error("row not deleted after fire")
	}
}

func TestOrphanedAfterRowsCleanedOnStart(t *testing.T) {
	t.Parallel()
	s, fired, db := newTestSched(t, time.UTC)
	ctx := t.Context()

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	for _, row := range [][2]string{
		{"s1|after|deadbeef", "after"},
		{"s1|every|1h|1", "every"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO timers (id, script_id, type, spec, next_run) VALUES (?, 's1', ?, '1h', ?)`,
			row[0], row[1], future); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if n := timerCount(t, db, "type = 'after'"); n != 0 {
		t.Errorf("%d orphaned after rows survived Start", n)
	}
	if n := timerCount(t, db, "type = 'every'"); n != 1 {
		t.Errorf("every row count = %d, want 1", n)
	}
	assertNoFire(t, fired, 50*time.Millisecond)
}

func TestPruneScript(t *testing.T) {
	t.Parallel()
	s, _, db := newTestSched(t, time.UTC)
	ctx := t.Context()

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	for _, id := range []string{"s1|every|1h|1", "s1|every|5m|1", "s1|at|07:00|1", "s2|every|1h|1"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO timers (id, script_id, type, spec, next_run) VALUES (?, ?, 'every', 'x', ?)`,
			id, id[:2], future); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.PruneScript(ctx, "s1", []string{"s1|every|1h|1"}); err != nil {
		t.Fatal(err)
	}
	if n := timerCount(t, db, "script_id = 's1'"); n != 1 {
		t.Errorf("s1 rows = %d, want 1", n)
	}
	if n := timerCount(t, db, "script_id = 's2'"); n != 1 {
		t.Errorf("s2 rows = %d, want 1 (other scripts must be untouched)", n)
	}

	// Empty keep wipes the script entirely.
	if err := s.PruneScript(ctx, "s1", nil); err != nil {
		t.Fatal(err)
	}
	if n := timerCount(t, db, "script_id = 's1'"); n != 0 {
		t.Errorf("s1 rows after empty prune = %d, want 0", n)
	}
}

func TestRemoveScript(t *testing.T) {
	t.Parallel()
	s, fired, db := newTestSched(t, time.UTC)
	ctx := t.Context()

	everyID, err := s.RegisterEvery(ctx, "s1", "20ms", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterAfter(ctx, "s1", "20ms"); err != nil {
		t.Fatal(err)
	}
	otherID, err := s.RegisterAfter(ctx, "s2", "30ms")
	if err != nil {
		t.Fatal(err)
	}

	s.RemoveScript("s1")

	// every/at rows survive (stable IDs carry state across reloads);
	// after rows die with their callbacks.
	if timerCount(t, db, "id = ?", everyID) != 1 {
		t.Error("every row deleted by RemoveScript")
	}
	if n := timerCount(t, db, "script_id = 's1' AND type = 'after'"); n != 0 {
		t.Errorf("s1 after rows = %d, want 0", n)
	}

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f := waitFire(t, fired)
	if f.timerID != otherID {
		t.Fatalf("fire = %+v, want only s2's timer", f)
	}
	assertNoFire(t, fired, 100*time.Millisecond)
}

func TestHeapOrdering(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	var h timerHeap
	perm := rand.Perm(100)
	for _, i := range perm {
		heap.Push(&h, &timer{
			id:      fmt.Sprintf("t%d", i),
			nextRun: base.Add(time.Duration(i) * time.Second),
		})
	}
	var got []time.Time
	for h.Len() > 0 {
		got = append(got, heap.Pop(&h).(*timer).nextRun)
	}
	if len(got) != 100 {
		t.Fatalf("popped %d timers, want 100", len(got))
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Before(got[j]) }) {
		t.Error("heap did not pop timers in nextRun order")
	}
}

func TestRegisterRejectsBadSpecs(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestSched(t, time.UTC)
	ctx := t.Context()

	for _, bad := range []string{"", "five minutes", "-5m", "0s"} {
		if _, err := s.RegisterEvery(ctx, "s1", bad, 1); err == nil {
			t.Errorf("RegisterEvery accepted %q", bad)
		}
		if _, err := s.RegisterAfter(ctx, "s1", bad); err == nil {
			t.Errorf("RegisterAfter accepted %q", bad)
		}
	}
	if _, err := s.RegisterAt(ctx, "s1", "7am", 1); err == nil {
		t.Error("RegisterAt accepted a bad spec")
	}
}

func BenchmarkSchedulerFire(b *testing.B) {
	s, _, _ := newTestSched(b, time.UTC)
	s.onFire = func(string, string) {}
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		if _, err := s.RegisterEvery(ctx, "bench", "1h", i+1); err != nil {
			b.Fatal(err)
		}
	}

	past := time.Now().UTC().Add(-time.Minute)
	for b.Loop() {
		b.StopTimer()
		s.mu.Lock()
		for _, t := range s.heap {
			t.nextRun = past
		}
		heap.Init(&s.heap)
		s.mu.Unlock()
		b.StartTimer()
		s.fireDue(ctx)
	}
}

func BenchmarkSchedulerConcurrencyStress(b *testing.B) {
	s, fired, _ := newTestSched(b, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Drain fired events in background to prevent blocking the scheduler
	go func() {
		for {
			select {
			case <-fired:
			case <-ctx.Done():
				return
			}
		}
	}()

	if err := s.Start(ctx); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		workerID := rand.IntN(100000)
		seq := 0
		for pb.Next() {
			seq++
			scriptID := fmt.Sprintf("s_%d", workerID)

			// 1. Register a recurring timer
			everyID, err := s.RegisterEvery(ctx, scriptID, "100ms", seq)
			if err != nil {
				b.Errorf("RegisterEvery failed: %v", err)
			}

			// 2. Register a one-shot timer
			afterID, err := s.RegisterAfter(ctx, scriptID, "10ms")
			if err != nil {
				b.Errorf("RegisterAfter failed: %v", err)
			}

			// 3. Prune timers
			if err := s.PruneScript(ctx, scriptID, []string{everyID, afterID}); err != nil {
				b.Errorf("PruneScript failed: %v", err)
			}

			// 4. Remove script (clears heap and after timers)
			s.RemoveScript(scriptID)
		}
	})
}
