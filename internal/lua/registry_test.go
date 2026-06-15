package lua

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/sztanpet/ha-lua/internal/ha"
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
