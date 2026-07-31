package lua

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/pprof"
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
	// Root sandboxes the scripts directory: the fs module (reads and writes),
	// require, and LoadAll's script enumeration. One process-wide handle,
	// shared across runners. May be nil in tests that never touch the
	// filesystem (fs/require then error, LoadAll fails).
	Root *os.Root
	// LogsRoot sandboxes ha.exceptions.log_file to the log directory. May be
	// nil (no log_dir configured — log_file then raises at load).
	LogsRoot    *os.Root
	NewKV       func(scriptID string) *store.Store
	CallService func(ctx context.Context, domain, service string, data jsontext.Value) error
	// CallServiceAsync backs ha.call_service{ wait = false }: ordered
	// synchronous send, HA's verdict on the returned channel. May be nil
	// (the binding then raises on wait=false).
	CallServiceAsync func(ctx context.Context, domain, service string, data jsontext.Value) (<-chan error, error)
	FireEvent        func(ctx context.Context, eventType string, data jsontext.Value) error
	SetState         func(ctx context.Context, entityID, state string, attrs jsontext.Value) (bool, error)
	RemoveState      func(ctx context.Context, entityID string) error
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

	r := NewRunner(id, s.scriptDir, s.deps.Root, s.deps.LogsRoot, s.deps.Tracker, s.deps.Scheduler, s.deps.NewKV(id), s.deps.Global)
	r.SetCallService(s.deps.CallService)
	r.SetCallServiceAsync(s.deps.CallServiceAsync)
	r.SetFireEvent(s.deps.FireEvent)
	r.SetSetState(s.deps.SetState)
	r.SetRemoveState(s.deps.RemoveState)

	sctx, cancel := context.WithCancel(ctx)
	h := &scriptHandle{runner: r, cancel: cancel, done: make(chan struct{})}
	s.scripts[id] = h
	s.reg.Add(r)

	path := filepath.Join(s.scriptDir, id+".lua")
	// The script id rides along in the label set, and labels are inherited by
	// every goroutine the runner spawns (and carried in the context it hands
	// to the Lua VM), so a CPU profile attributes work to the script that
	// caused it instead of to one anonymous pile of gopher-lua frames.
	labels := pprof.Labels("goroutine", "script", "script", id)
	s.wg.Add(1)
	go pprof.Do(sctx, labels, func(ctx context.Context) {
		defer s.wg.Done()
		defer close(h.done)
		r.Start(ctx, path)
	})
	if s.deps.Router != nil || s.deps.OnLoaded != nil {
		go pprof.Do(sctx, pprof.Labels("goroutine", "script-load", "script", id), func(ctx context.Context) {
			select {
			case <-r.LoadedCh:
				s.afterLoad(id, h, r)
			case <-ctx.Done():
			}
		})
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
	// Dropping the handle from s.scripts and unregistering the runner must be
	// ONE atomic transition, mirroring StartScript's add. With the removals
	// outside the lock, a StartScript racing in behind us saw an empty
	// s.scripts, installed a fresh runner in both maps, and then our stale
	// removals unregistered *its* runner: a live script tracked by the
	// supervisor but absent from the Registry, so it got no events, no timers,
	// and 404'd in the UI — permanently, because s.scripts still tracked it and
	// every later StartScript/Reload was therefore a no-op.
	//
	// Registry.Remove still blocks until in-flight dispatches finish, so once
	// this section returns nobody can Send to the runner and closing its
	// channel is safe. Only the drain wait stays outside the lock: it can take
	// stopTimeout, and no other script should be held up that long.
	s.mu.Lock()
	h, ok := s.scripts[id]
	if ok {
		delete(s.scripts, id)
		if s.deps.Router != nil {
			s.deps.Router.Unregister(id)
		}
		s.reg.Remove(id)
		s.deps.Scheduler.RemoveScript(id)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	stopScriptHook()

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

// stopScriptHook runs between StopScript's critical section and the drain wait.
// A no-op in production; the concurrency test swaps it in to park a stop in
// exactly the window where the removals used to live, which is what made a
// racing StartScript observable.
var stopScriptHook = func() {}

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
