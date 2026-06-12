package lua

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
)

// Event is the union type delivered to script goroutines.
type Event struct {
	HAEvent    *ha.Event
	TimerFired *TimerFiredEvent
}

// TimerFiredEvent is sent to a script goroutine when a timer fires.
type TimerFiredEvent struct {
	TimerID string
}

// Runner owns a single *lua.LState and its event channel.
type Runner struct {
	scriptID  string
	scriptDir string
	ch        chan Event
	timerFns  map[string]*lua.LFunction

	// LoadedCh is closed once the script has finished loading.
	LoadedCh chan struct{}
	// cachedEventHandlers is set after load; safe to read once LoadedCh is closed.
	cachedEventHandlers []eventHandler

	tracker *state.Tracker
	kv      *store.Store
	global  *store.GlobalStore

	// Wired by caller after construction; nil = not yet connected.
	callService func(ctx context.Context, domain, service string, data jsontext.Value) error
	fireEvent   func(ctx context.Context, eventType string, data jsontext.Value) error
}

// NewRunner creates a Runner. Call Start to load and run the script.
func NewRunner(scriptID, scriptDir string, tracker *state.Tracker, kv *store.Store, global *store.GlobalStore) *Runner {
	return &Runner{
		scriptID:  scriptID,
		scriptDir: scriptDir,
		ch:        make(chan Event, 64),
		LoadedCh:  make(chan struct{}),
		timerFns:  make(map[string]*lua.LFunction),
		tracker:   tracker,
		kv:        kv,
		global:    global,
	}
}

// ScriptID returns the script ID.
func (r *Runner) ScriptID() string { return r.scriptID }

// SetCallService wires the call_service function. Must be called before Start.
func (r *Runner) SetCallService(fn func(ctx context.Context, domain, service string, data jsontext.Value) error) {
	r.callService = fn
}

// SetFireEvent wires the fire_event function. Must be called before Start.
func (r *Runner) SetFireEvent(fn func(ctx context.Context, eventType string, data jsontext.Value) error) {
	r.fireEvent = fn
}

// eventHandlers returns the registered event handlers. Only valid after Start
// has loaded the script.
func (r *Runner) eventHandlers() []eventHandler {
	return r.cachedEventHandlers
}

// Send delivers an event to the script goroutine (non-blocking).
func (r *Runner) Send(ev Event) {
	select {
	case r.ch <- ev:
	default:
		slog.Warn("lua: event channel full, dropping", "script", r.scriptID)
	}
}

// SendHAEvent is a convenience wrapper for HA events.
func (r *Runner) SendHAEvent(ev ha.Event) {
	r.Send(Event{HAEvent: &ev})
}

// Start loads the script and begins the event loop. Blocks until ctx is done.
func (r *Runner) Start(ctx context.Context, scriptPath string) {
	L := r.newLState(ctx)
	defer L.Close()

	api := &haAPI{
		scriptID:    r.scriptID,
		tracker:     r.tracker,
		callService: r.callService,
		fireEvent:   r.fireEvent,
	}
	r.registerHaAPI(L, api)
	registerStoreAPI(L, r.kv, r.global)

	if err := L.DoFile(scriptPath); err != nil {
		slog.Error("lua: script load error", "script", r.scriptID, "err", err)
	}

	// Cache event handlers for Registry.EventTypes() and close the loaded signal.
	r.cachedEventHandlers = api.eventHandlers
	close(r.LoadedCh)

	// Deliver initial states for on_state_change with initial=true
	r.deliverInitialStates(ctx, L, api)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-r.ch:
			if !ok {
				return
			}
			r.handleEvent(L, api, ev)
		}
	}
}

func (r *Runner) deliverInitialStates(ctx context.Context, L *lua.LState, api *haAPI) {
	for _, h := range api.stateChangeHandlers {
		if !h.initial {
			continue
		}
		entities, err := r.tracker.GetEntities(ctx, h.pattern)
		if err != nil {
			slog.Warn("lua: initial state query failed", "script", r.scriptID, "err", err)
			continue
		}
		for i := range entities {
			s := &entities[i]
			ev := ha.Event{
				Type: "state_changed",
				Data: buildInitialEventData(s),
			}
			evTbl := eventToLua(L, ev)
			dataTbl, _ := evTbl.RawGetString("data").(*lua.LTable)
			callProtected(L, api, "state_changed", evTbl, h.fn, dataTbl)
		}
	}
}

func (r *Runner) handleEvent(L *lua.LState, api *haAPI, ev Event) {
	if ev.HAEvent != nil {
		r.handleHAEvent(L, api, *ev.HAEvent)
	}
	if ev.TimerFired != nil {
		r.handleTimerFired(L, ev.TimerFired.TimerID)
	}
}

func (r *Runner) handleHAEvent(L *lua.LState, api *haAPI, ev ha.Event) {
	evTbl := eventToLua(L, ev)

	if ev.Type == "state_changed" {
		dataTbl, _ := evTbl.RawGetString("data").(*lua.LTable)
		var entityID string
		if dataTbl != nil {
			if v := dataTbl.RawGetString("entity_id"); v != lua.LNil {
				entityID = v.String()
			}
		}
		for _, h := range api.stateChangeHandlers {
			matched, _ := filepath.Match(h.pattern, entityID)
			if matched {
				callProtected(L, api, "state_changed", evTbl, h.fn, dataTbl)
			}
		}
		return
	}

	for _, h := range api.eventHandlers {
		if h.eventType == ev.Type {
			dataTbl, _ := evTbl.RawGetString("data").(*lua.LTable)
			callProtected(L, api, "event", evTbl, h.fn, dataTbl)
		}
	}
}

func (r *Runner) handleTimerFired(L *lua.LState, timerID string) {
	// Timer dispatch is wired in after milestone 8 adds the scheduler API.
	// Placeholder: runners created before that milestone have no timer fns.
	_ = L
	_ = timerID
}

// newLState creates a Lua state with a basic set of libraries.
// Full sandboxing (SkipOpenLibs + selective open) is applied in milestone 10.
func (r *Runner) newLState(ctx context.Context) *lua.LState {
	L := lua.NewState()
	installRestrictedRequire(L, r.scriptDir)
	L.SetContext(ctx)
	return L
}

// buildInitialEventData constructs a minimal state_changed data payload
// for the opts.initial delivery (old_state is absent).
func buildInitialEventData(s *ha.StateData) jsontext.Value {
	type stateObj struct {
		EntityID   string         `json:"entity_id"`
		State      string         `json:"state"`
		Attributes jsontext.Value `json:"attributes"`
	}
	type evData struct {
		EntityID string   `json:"entity_id"`
		NewState stateObj `json:"new_state"`
	}
	attrs := s.Attributes
	if len(attrs) == 0 {
		attrs = jsontext.Value("{}")
	}
	d := evData{
		EntityID: s.EntityID,
		NewState: stateObj{
			EntityID:   s.EntityID,
			State:      s.State,
			Attributes: attrs,
		},
	}
	b, _ := json.Marshal(d)
	return jsontext.Value(b)
}
