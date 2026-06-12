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
	"time"
)

// Purger deletes expired state_history rows on a fixed interval.
type Purger struct {
	db            *sql.DB
	retentionDays int
	interval      time.Duration
}

// New creates a Purger. db must be the write handle.
func New(db *sql.DB, retentionDays int, interval time.Duration) *Purger {
	return &Purger{db: db, retentionDays: retentionDays, interval: interval}
}

// Start runs the purge loop in a background goroutine until ctx is
// cancelled. One purge runs immediately: with the default 1h interval a
// frequently restarted daemon would otherwise never reach its first tick.
func (p *Purger) Start(ctx context.Context) {
	go func() {
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
	}()
}

// RunOnce deletes all state_history rows older than the retention
// window. The cutoff must be computed here and bound as RFC3339:
// changed_at is RFC3339 TEXT ('T' separator, 'Z' suffix) while SQLite's
// datetime('now',...) renders 'YYYY-MM-DD HH:MM:SS', and under plain
// string comparison ' ' < 'T' makes same-day rows never compare
// less-than — the purge would silently lag by up to a day.
func (p *Purger) RunOnce(ctx context.Context) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -p.retentionDays).Format(time.RFC3339)
	res, err := p.db.ExecContext(ctx, `DELETE FROM state_history WHERE changed_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("purge state_history: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		slog.Info("purge: deleted expired history rows", "rows", n, "cutoff", cutoff)
	}
	return nil
}
