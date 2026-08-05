// Package web serves the script-driven HTTP UI on a plain LAN port.
package web

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/pprof"
	"time"
)

// Start runs an HTTP server on addr backed by handler until ctx is cancelled.
// No-op if addr is empty (UI server disabled).
func Start(ctx context.Context, addr string, handler http.Handler) {
	if addr == "" {
		return
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// addr is part of the label set: LAN and ingress run two servers over
	// the same handler, and a profile is useless if it cannot tell them apart.
	labels := pprof.Labels("goroutine", "web", "addr", addr)
	go pprof.Do(ctx, labels, func(context.Context) {
		slog.Info("web: UI server starting", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("web: server error", "err", err)
		}
	})
	go pprof.Do(ctx, labels, func(ctx context.Context) {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
}
