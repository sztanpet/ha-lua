package lua

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

func BenchmarkRegistryDispatch(b *testing.B) {
	slog.SetLogLoggerLevel(slog.LevelError)
	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	numRunners := 100
	for i := 0; i < numRunners; i++ {
		r := &Runner{
			scriptID: fmt.Sprintf("s%d", i),
			ch:       make(chan Event, 10),
		}
		reg.Add(r)
		// Drain events in background
		go func(ch chan Event) {
			for {
				select {
				case <-ch:
				case <-ctx.Done():
					return
				}
			}
		}(r.ch)
	}

	ev := ha.Event{Type: "state_changed"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.Dispatch(ev)
	}
}

// TestDispatchSkipsScriptsWithNoHandler: a script is woken only for events it
// can act on. An MQTT-only script used to be queued every state change in the
// house, dispatch each one into an empty handler loop, and log a line about
// it.
func TestDispatchSkipsScriptsWithNoHandler(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "quiet.lua", `ha.on_event("my_event", function() end)`)

	writeDB, readDB := testutil.NewTestDB(t, func(w, _ *sql.DB) error { return state.Migrate(w) })
	reg := NewRegistry()
	r := NewRunner("quiet", dir, openTestRoot(t, dir), nil, nil, nil,
		store.New(writeDB, readDB, "quiet"), store.NewGlobal(writeDB, readDB))
	reg.Add(r)

	done := make(chan struct{})
	go func() { defer close(done); r.Start(t.Context(), filepath.Join(dir, "quiet.lua")) }()
	t.Cleanup(func() { <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("script did not load")
	}

	if r.wantsHAEvent(ha.Event{Type: "state_changed"}) {
		t.Error("a script with no state handler still wants state_changed")
	}
	if !r.wantsHAEvent(ha.Event{Type: "my_event"}) {
		t.Error("a script dropped the event type it registered for")
	}
	if r.wantsHAEvent(ha.Event{Type: "someone_elses_event"}) {
		t.Error("a script wants an event type nobody registered for")
	}
}
