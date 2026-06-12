package lua

import (
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
// The Send happens under the read lock: a runner's channel is closed only
// after Remove returns, and Remove blocks on this lock, so the channel
// cannot be closed out from under us.
func (reg *Registry) DispatchToTimer(scriptID, timerID string) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	r := reg.runners[scriptID]
	if r == nil {
		slog.Warn("lua: timer fired for unknown script", "script", scriptID, "timer", timerID)
		return
	}
	r.Send(Event{TimerFired: &TimerFiredEvent{TimerID: timerID}})
}
