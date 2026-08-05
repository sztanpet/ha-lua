package lua

import (
	"log/slog"
	"sort"
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

// All returns a snapshot of all runners, ordered by script id. The order is
// part of the contract: the debug page re-renders this list every few seconds,
// and map order would reshuffle it under the reader's eyes.
func (reg *Registry) All() []*Runner {
	reg.mu.RLock()
	out := make([]*Runner, 0, len(reg.runners))
	for _, r := range reg.runners {
		out = append(out, r)
	}
	reg.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].scriptID < out[j].scriptID })
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
