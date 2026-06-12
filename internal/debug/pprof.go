package debug

import (
	"context"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"time"
)

// Start starts an HTTP pprof server on addr. No-op if addr is empty.
// The server shuts down when ctx is cancelled.
func Start(ctx context.Context, addr string) {
	if addr == "" {
		return
	}
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)

	srv := &http.Server{Addr: addr}
	go func() {
		slog.Info("debug: pprof server starting", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("debug: pprof server error", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
}
