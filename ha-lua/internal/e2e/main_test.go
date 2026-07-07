package e2e

import (
	"log/slog"
	"os"
	"testing"
)

// TestMain quiets slog: the client logs INFO lines (auth, reconnect) that
// interleave with benchmark output, and teardown cancels a context that a
// parked call_service then reports at ERROR. Neither is signal here.
func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError + 4) // above every level: silence
	os.Exit(m.Run())
}
