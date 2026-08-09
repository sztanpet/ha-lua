// Package purge deletes state_history rows older than the retention
// window. HA's own recorder handles long-term history; ours exists for
// short-window ha.get_history queries, so a simple periodic DELETE is
// all this needs to be.
package purge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"runtime/pprof"
	"time"
)

// Rule keeps entities matching Pattern (a SQLite GLOB) for Days instead of the
// default retention.
type Rule struct {
	Pattern string
	Days    int
}

// Purger deletes expired state_history rows on a fixed interval.
type Purger struct {
	db            *sql.DB
	retentionDays int
	interval      time.Duration
	rules         []Rule
}

// New creates a Purger. db must be the write handle. Rules override the
// default retention per entity glob, first match winning; malformed ones are
// dropped with a warning rather than taken as a reason not to purge at all.
func New(db *sql.DB, retentionDays int, interval time.Duration, rules ...Rule) *Purger {
	kept := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.Pattern == "" || r.Days <= 0 {
			slog.Warn("purge: ignoring malformed retention rule",
				"pattern", r.Pattern, "days", r.Days)
			continue
		}
		kept = append(kept, r)
	}
	return &Purger{db: db, retentionDays: retentionDays, interval: interval, rules: kept}
}

// Start runs the purge loop in a background goroutine until ctx is
// cancelled. One purge runs immediately: with the default 1h interval a
// frequently restarted daemon would otherwise never reach its first tick.
func (p *Purger) Start(ctx context.Context) {
	go pprof.Do(ctx, pprof.Labels("goroutine", "purge"), func(ctx context.Context) {
		if err := p.RunOnce(ctx); err != nil {
			slog.Warn("purge failed", "err", err)
		}
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.RunOnce(ctx); err != nil {
					slog.Warn("purge failed", "err", err)
				}
			}
		}
	})
}

// RunOnce deletes all state_history rows past their retention window: one
// DELETE per rule, then one for everything no rule claimed. The cutoff must be
// computed here and bound as RFC3339: changed_at is RFC3339 TEXT ('T'
// separator, 'Z' suffix) while SQLite's datetime('now',...) renders
// 'YYYY-MM-DD HH:MM:SS', and under plain string comparison ' ' < 'T' makes
// same-day rows never compare less-than — the purge would silently lag by up
// to a day.
func (p *Purger) RunOnce(ctx context.Context) error {
	now := time.Now().UTC()
	total := int64(0)

	// Each rule excludes the ones before it, so the first match wins whichever
	// window is longer — overlapping globs must not have the most aggressive
	// rule quietly delete what an earlier one promised to keep.
	for i, rule := range p.rules {
		query := `DELETE FROM state_history WHERE entity_id GLOB ? AND changed_at < ?`
		args := []any{rule.Pattern, now.AddDate(0, 0, -rule.Days).Format(time.RFC3339)}
		for _, earlier := range p.rules[:i] {
			query += ` AND entity_id NOT GLOB ?`
			args = append(args, earlier.Pattern)
		}
		n, err := p.exec(ctx, query, args...)
		if err != nil {
			return err
		}
		total += n
	}

	query := `DELETE FROM state_history WHERE changed_at < ?`
	args := []any{now.AddDate(0, 0, -p.retentionDays).Format(time.RFC3339)}
	for _, rule := range p.rules {
		query += ` AND entity_id NOT GLOB ?`
		args = append(args, rule.Pattern)
	}
	n, err := p.exec(ctx, query, args...)
	if err != nil {
		return err
	}
	total += n

	if total > 0 {
		slog.Info("purge: deleted expired history rows", "rows", total)
	}
	return nil
}

func (p *Purger) exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("purge state_history: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
