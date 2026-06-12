package lua

import (
	"context"
	"log/slog"
	"sync"

	"github.com/sztanpet/ha-lua/internal/ha"
)

// Registry manages all running script runners and routes events to them.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]*Runner
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{runners: make(map[string]*Runner)}
}

// Add registers a runner. The script ID must be unique.
func (reg *Registry) Add(r *Runner) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.runners[r.scriptID] = r
}

// Remove removes a runner by script ID.
func (reg *Registry) Remove(scriptID string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	delete(reg.runners, scriptID)
}

// Get returns the runner for scriptID, or nil.
func (reg *Registry) Get(scriptID string) *Runner {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return reg.runners[scriptID]
}

// All returns a snapshot of all runners.
func (reg *Registry) All() []*Runner {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	out := make([]*Runner, 0, len(reg.runners))
	for _, r := range reg.runners {
		out = append(out, r)
	}
	return out
}

// Dispatch fans out an HA event to all registered runners. Non-blocking per
// runner (Send drops events if the channel is full).
func (reg *Registry) Dispatch(ev ha.Event) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, r := range reg.runners {
		r.SendHAEvent(ev)
	}
}

// DispatchToTimer sends a TimerFiredEvent to the runner for scriptID.
func (reg *Registry) DispatchToTimer(scriptID, timerID string) {
	reg.mu.RLock()
	r := reg.runners[scriptID]
	reg.mu.RUnlock()
	if r == nil {
		slog.Warn("lua: timer fired for unknown script", "script", scriptID, "timer", timerID)
		return
	}
	r.Send(Event{TimerFired: &TimerFiredEvent{TimerID: timerID}})
}

// EventTypes collects the distinct non-state_changed event types needed
// by all registered scripts.
func (reg *Registry) EventTypes() []string {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, r := range reg.runners {
		for _, h := range r.eventHandlers() {
			seen[h.eventType] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out
}

// RunAll spawns each runner in its own goroutine. Blocks until ctx is done
// and all runners have stopped.
func (reg *Registry) RunAll(ctx context.Context, scriptPaths map[string]string) {
	reg.mu.RLock()
	runners := make([]*Runner, 0, len(reg.runners))
	for _, r := range reg.runners {
		runners = append(runners, r)
	}
	reg.mu.RUnlock()

	var wg sync.WaitGroup
	for _, r := range runners {
		path, ok := scriptPaths[r.scriptID]
		if !ok {
			slog.Warn("lua: no script path for runner", "script", r.scriptID)
			continue
		}
		wg.Add(1)
		go func(runner *Runner, p string) {
			defer wg.Done()
			runner.Start(ctx, p)
		}(r, path)
	}
	wg.Wait()
}
