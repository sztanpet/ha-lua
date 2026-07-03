package lua

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
)

// stopTimeout is how long a script gets to drain its event channel before
// its context is cancelled, which aborts the Lua VM mid-callback.
const stopTimeout = 5 * time.Second

// Deps are the shared subsystems every script runner is wired with.
type Deps struct {
	Tracker   *state.Tracker
	Scheduler *scheduler.Scheduler
	Global    *store.GlobalStore
	// Root sandboxes all reads under the scripts directory: the fs module,
	// require, and LoadAll's script enumeration. One process-wide handle,
	// shared across runners. May be nil in tests that never touch the
	// filesystem (fs/require then error, LoadAll fails).
	Root        *os.Root
	NewKV       func(scriptID string) *store.Store
	CallService func(ctx context.Context, domain, service string, data jsontext.Value) error
	FireEvent   func(ctx context.Context, eventType string, data jsontext.Value) error
	SetState    func(ctx context.Context, entityID, state string, attrs jsontext.Value) (bool, error)
	RemoveState func(ctx context.Context, entityID string) error
	// Router receives each script's ha.serve routes on load and loses them on
	// stop. May be nil (no UI server).
	Router *Router
	// OnLoaded is called (on its own goroutine) once a started script has
	// finished loading — the hook for subscribing newly required event
	// types. May be nil.
	OnLoaded func(r *Runner)
}

// Supervisor owns the lifecycle of all script runners: initial load,
// stop, and hot reload. All state transitions go through it so a script
// is never registered twice and never receives events while stopping.
type Supervisor struct {
	reg       *Registry
	scriptDir string
	deps      Deps

	mu      sync.Mutex
	scripts map[string]*scriptHandle
	wg      sync.WaitGroup
}

type scriptHandle struct {
	runner *Runner
	cancel context.CancelFunc
	done   chan struct{}
}

// NewSupervisor creates a Supervisor managing scripts in scriptDir.
func NewSupervisor(reg *Registry, scriptDir string, deps Deps) *Supervisor {
	return &Supervisor{
		reg:       reg,
		scriptDir: scriptDir,
		deps:      deps,
		scripts:   make(map[string]*scriptHandle),
	}
}

// LoadAll starts every *.lua script in the script directory. Enumeration goes
// through the shared os.Root — the same handle backing fs.read and require —
// so all reads under the scripts dir take one rooted-IO path.
func (s *Supervisor) LoadAll(ctx context.Context) error {
	if s.deps.Root == nil {
		return fmt.Errorf("read scripts dir: no scripts root")
	}
	entries, err := fs.ReadDir(s.deps.Root.FS(), ".")
	if err != nil {
		return fmt.Errorf("read scripts dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".lua") || strings.HasPrefix(name, ".") {
			continue
		}
		s.StartScript(ctx, strings.TrimSuffix(name, ".lua"))
	}
	return nil
}

// StartScript creates a runner for id and spawns its goroutine. No-op if
// the script is already running.
func (s *Supervisor) StartScript(ctx context.Context, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.scripts[id]; ok {
		return
	}

	r := NewRunner(id, s.scriptDir, s.deps.Root, s.deps.Tracker, s.deps.Scheduler, s.deps.NewKV(id), s.deps.Global)
	r.SetCallService(s.deps.CallService)
	r.SetFireEvent(s.deps.FireEvent)
	r.SetSetState(s.deps.SetState)
	r.SetRemoveState(s.deps.RemoveState)

	sctx, cancel := context.WithCancel(ctx)
	h := &scriptHandle{runner: r, cancel: cancel, done: make(chan struct{})}
	s.scripts[id] = h
	s.reg.Add(r)

	path := filepath.Join(s.scriptDir, id+".lua")
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(h.done)
		r.Start(sctx, path)
	}()
	if s.deps.Router != nil || s.deps.OnLoaded != nil {
		go func() {
			select {
			case <-r.LoadedCh:
				s.afterLoad(id, h, r)
			case <-sctx.Done():
			}
		}()
	}
}

// afterLoad runs once a script has loaded: it registers the script's UI routes
// and fires the OnLoaded hook. Route registration holds s.mu across the
// handle-identity check and the Register call, so it is fully serialized with
// StopScript's Unregister (also under s.mu) — a concurrent stop/reload can
// never leave a dangling mapping.
func (s *Supervisor) afterLoad(id string, h *scriptHandle, r *Runner) {
	if s.deps.Router != nil {
		s.mu.Lock()
		if s.scripts[id] == h {
			s.deps.Router.Register(id, r.Routes())
		}
		s.mu.Unlock()
	}
	if s.deps.OnLoaded != nil {
		s.deps.OnLoaded(r)
	}
}

// StopScript removes the script from event dispatch, lets it drain its
// queued events, and waits for its goroutine to exit. Scripts stuck in a
// callback past stopTimeout get their context cancelled, which aborts
// the Lua VM. No-op if the script is not running.
func (s *Supervisor) StopScript(id string) {
	s.mu.Lock()
	h, ok := s.scripts[id]
	if ok {
		delete(s.scripts, id)
		if s.deps.Router != nil {
			s.deps.Router.Unregister(id)
		}
	}
	s.mu.Unlock()
	if !ok {
		return
	}

	// Remove blocks until in-flight dispatches finish, so once it
	// returns nobody can Send to this runner and closing its channel
	// is safe.
	s.reg.Remove(id)
	s.deps.Scheduler.RemoveScript(id)
	h.runner.Close()
	select {
	case <-h.done:
	case <-time.After(stopTimeout):
		slog.Warn("lua: script slow to stop, aborting its VM", "script", id)
		h.cancel()
		<-h.done
	}
	h.cancel()
}

// Reload restarts the script from its current file, or starts it if it
// was not running (a newly created file).
func (s *Supervisor) Reload(ctx context.Context, id string) {
	s.StopScript(id)
	s.StartScript(ctx, id)
}

// Wait blocks until all script goroutines have exited.
func (s *Supervisor) Wait() {
	s.wg.Wait()
}
