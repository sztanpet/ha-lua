// Package lua owns the per-script LState lifecycle and all Lua API
// bindings: ha.*, store.*, global.*, and the restricted require.
package lua

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
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

// batchWindow is how long HA events are coalesced before a script's handlers
// run. state_changed events for the same entity within a window collapse to the
// newest (a chatty sensor that flaps 50×/s becomes one dispatch per window);
// other events are kept in order. Timers and a script that calls
// ha.immediate_events() bypass batching. The small added latency keeps a burst
// of events from overflowing the per-script channel and dropping.
const batchWindow = 100 * time.Millisecond

// haEventCoalesceKey returns the per-entity key for a state_changed event (so
// only the latest is kept within a window) and whether the event coalesces at
// all. Every non-state_changed event keeps its own slot.
func haEventCoalesceKey(ev Event) (string, bool) {
	if ev.HAEvent == nil || ev.HAEvent.Type != "state_changed" {
		return "", false
	}
	var d struct {
		EntityID string `json:"entity_id"`
	}
	if err := json.Unmarshal(ev.HAEvent.Data, &d); err != nil || d.EntityID == "" {
		return "", false
	}
	return d.EntityID, true
}

// Runner owns a single *lua.LState and its event channel.
type Runner struct {
	scriptID  string
	scriptDir string
	// root sandboxes the fs module to scriptDir; shared (os.Root is
	// goroutine-safe), may be nil in tests that don't exercise fs.
	root *os.Root
	// logsRoot sandboxes ha.exceptions.log_file to the log directory; shared,
	// may be nil (no log_dir configured — log_file then raises at load).
	logsRoot *os.Root
	ch       chan Event
	// reqCh delivers UI HTTP requests. Unlike ch (lossy fan-out, dropped when
	// full), requests block the sender up to the request timeout and are never
	// dropped. reqCh is never closed, so the Router's send can only block
	// (bounded by its deadline), never panic.
	reqCh    chan *request
	timerFns map[string]*lua.LFunction

	// LoadedCh is closed once the script has finished loading.
	LoadedCh chan struct{}
	// cachedEventHandlers is set after load; safe to read once LoadedCh is closed.
	cachedEventHandlers []eventHandler
	// cachedRoutes is set after load; safe to read once LoadedCh is closed.
	cachedRoutes []RouteSpec

	tracker   *state.Tracker
	scheduler *scheduler.Scheduler
	kv        *store.Store
	global    *store.GlobalStore

	// Wired by caller after construction; nil = not yet connected.
	callService func(ctx context.Context, domain, service string, data jsontext.Value) error
	fireEvent   func(ctx context.Context, eventType string, data jsontext.Value) error
	setState    func(ctx context.Context, entityID, state string, attrs jsontext.Value) (bool, error)
	removeState func(ctx context.Context, entityID string) error
}

// NewRunner creates a Runner. Call Start to load and run the script.
func NewRunner(scriptID, scriptDir string, root, logsRoot *os.Root, tracker *state.Tracker, scheduler *scheduler.Scheduler, kv *store.Store, global *store.GlobalStore) *Runner {
	return &Runner{
		scriptID:  scriptID,
		scriptDir: scriptDir,
		root:      root,
		logsRoot:  logsRoot,
		ch:        make(chan Event, 256),
		reqCh:     make(chan *request),
		LoadedCh:  make(chan struct{}),
		timerFns:  make(map[string]*lua.LFunction),
		tracker:   tracker,
		scheduler: scheduler,
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

// SetSetState wires the set_state function. Must be called before Start.
func (r *Runner) SetSetState(fn func(ctx context.Context, entityID, state string, attrs jsontext.Value) (bool, error)) {
	r.setState = fn
}

// SetRemoveState wires the remove_state function. Must be called before Start.
func (r *Runner) SetRemoveState(fn func(ctx context.Context, entityID string) error) {
	r.removeState = fn
}

// EventTypes returns the distinct custom event types this script handles.
// Only valid once LoadedCh is closed.
func (r *Runner) EventTypes() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, h := range r.cachedEventHandlers {
		if _, ok := seen[h.eventType]; ok {
			continue
		}
		seen[h.eventType] = struct{}{}
		out = append(out, h.eventType)
	}
	return out
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

// Close closes the event channel: Start drains whatever is queued and
// returns. The runner must be removed from the registry first — Send on
// a closed channel panics. Call exactly once.
func (r *Runner) Close() {
	close(r.ch)
}

// Start loads the script and begins the event loop. Blocks until ctx is done.
func (r *Runner) Start(ctx context.Context, scriptPath string) {
	L := r.newLState(ctx)
	defer L.Close()

	api := &haAPI{
		scriptID:    r.scriptID,
		tracker:     r.tracker,
		scheduler:   r.scheduler,
		callService: r.callService,
		fireEvent:   r.fireEvent,
		setState:    r.setState,
		removeState: r.removeState,
		timerFns:    make(map[string]*lua.LFunction),
	}
	r.registerHaAPI(L, api)
	registerStoreAPI(L, r.kv, r.global)

	if err := L.DoFile(scriptPath); err != nil {
		slog.Error("lua: script load error", "script", r.scriptID, "err", err)
	}

	// Persist timer functions for dispatch and prune old rows.
	r.timerFns = api.timerFns
	if r.scheduler != nil {
		if err := r.scheduler.PruneScript(ctx, r.scriptID, api.keepIDs); err != nil {
			slog.Warn("lua: timer pruning failed", "script", r.scriptID, "err", err)
		}
	}

	// Cache event handlers and routes for the supervisor/router, then signal
	// loaded. Both are safe to read once LoadedCh is closed.
	r.cachedEventHandlers = api.eventHandlers
	r.cachedRoutes = api.routeSpecs()
	close(r.LoadedCh)

	// Deliver initial states for on_state_change with initial=true
	r.deliverInitialStates(ctx, L, api)

	// Events are coalesced over batchWindow before dispatch, unless the script
	// opted into immediate delivery. pending holds the current window; coalesceIdx
	// maps an entity to its slot so a repeated state_changed overwrites in place.
	immediate := api.immediateEvents
	var pending []Event
	coalesceIdx := make(map[string]int)
	var flushC <-chan time.Time

	flush := func() {
		for i := range pending {
			r.handleEvent(L, api, pending[i])
		}
		pending = pending[:0]
		clear(coalesceIdx)
		flushC = nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case req := <-r.reqCh:
			r.handleRequest(L, api, req)
		case ev, ok := <-r.ch:
			if !ok {
				flush() // channel closed: dispatch the final window, then stop
				return
			}
			// Timers fire on a precise schedule, and immediate mode wants no
			// delay; both bypass batching. Drain any pending window first so a
			// batched event never overtakes one delivered immediately.
			if immediate || ev.TimerFired != nil {
				flush()
				r.handleEvent(L, api, ev)
				continue
			}
			if key, coalesce := haEventCoalesceKey(ev); coalesce {
				if idx, seen := coalesceIdx[key]; seen {
					pending[idx] = ev // keep only the newest state for this entity
					continue
				}
				coalesceIdx[key] = len(pending)
			}
			pending = append(pending, ev)
			if flushC == nil {
				// A fresh timer per window avoids the Timer.Reset drain footgun.
				flushC = time.After(batchWindow)
			}
		case <-flushC:
			flush()
		}
	}
}

// Routes returns the routes this script registered via ha.serve. Only valid
// once LoadedCh is closed.
func (r *Runner) Routes() []RouteSpec { return r.cachedRoutes }

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

// warnDispatchDelay is the queue-to-handler delay above which an event gets a
// warning instead of a debug line. It sits well above the batch window, so a
// warning always means something real: a parked event loop (a handler blocked
// in a synchronous call) or a backed-up channel.
const warnDispatchDelay = 250 * time.Millisecond

func (r *Runner) handleEvent(L *lua.LState, api *haAPI, ev Event) {
	if ev.HAEvent != nil {
		if !ev.HAEvent.ReceivedAt.IsZero() {
			delay := time.Since(ev.HAEvent.ReceivedAt)
			if delay >= warnDispatchDelay {
				slog.Warn("lua: event waited long for its handler",
					"script", r.scriptID, "event", ev.HAEvent.Type, "delay", delay)
			} else {
				slog.Debug("lua: event dispatch delay",
					"script", r.scriptID, "event", ev.HAEvent.Type, "delay", delay)
			}
		}
		r.handleHAEvent(L, api, *ev.HAEvent)
	}
	if ev.TimerFired != nil {
		r.handleTimerFired(L, api, ev.TimerFired.TimerID)
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

func (r *Runner) handleTimerFired(L *lua.LState, api *haAPI, timerID string) {
	fn, ok := r.timerFns[timerID]
	if !ok {
		return
	}

	callback, isAfter := timerCallbackName(r.scriptID, timerID)
	if isAfter {
		delete(r.timerFns, timerID)
	}
	callProtected(L, api, callback, nil, fn)
}

// timerCallbackName maps a timer ID ("<script_id>|<type>|…") to the
// exception-callback label and reports whether it is a one-shot "after"
// timer. The script ID is a filename that may itself contain '|', so the
// known prefix is stripped rather than splitting the ID blind.
func timerCallbackName(scriptID, timerID string) (name string, isAfter bool) {
	rest := strings.TrimPrefix(timerID, scriptID+"|")
	typ, _, ok := strings.Cut(rest, "|")
	if !ok || typ == "" {
		return "timer", false
	}
	return "timer_" + typ, typ == "after"
}

// handleRequest runs a UI request handler on the script goroutine and replies.
// The reply channel is buffered (cap 1), so the send never blocks even if the
// client already timed out.
func (r *Runner) handleRequest(L *lua.LState, api *haAPI, req *request) {
	resp := response{status: http.StatusNotFound, body: "not found"}
	defer func() { req.reply <- resp }()

	fn := api.matchRoute(req.method, req.path)
	if fn == nil {
		return
	}

	reqTbl := requestToLua(L, req)
	out, err := r.callHandler(L, api, req.method+" "+req.path, fn, reqTbl)
	if err != nil {
		resp = response{status: http.StatusInternalServerError, body: "internal error"}
		return
	}
	resp = out
}

// callHandler invokes a route handler protected, reads its (status, body,
// headers) return values defensively, and routes any error to on_exception.
func (r *Runner) callHandler(L *lua.LState, api *haAPI, name string, fn *lua.LFunction, arg lua.LValue) (response, error) {
	if err := L.CallByParam(lua.P{Fn: fn, NRet: 3, Protect: true}, arg); err != nil {
		errMsg, traceback := luaErrParts(err)
		dispatchException(L, api, errMsg, traceback, name, nil)
		return response{}, err
	}
	// PCall pads to exactly NRet results, so all three slots exist (nil if the
	// handler returned fewer). Parse defensively: a garbage return is a 200,
	// never a panic.
	statusV := L.Get(-3)
	bodyV := L.Get(-2)
	headersV := L.Get(-1)
	L.Pop(3)

	resp := response{status: http.StatusOK}
	if n, ok := statusV.(lua.LNumber); ok {
		resp.status = int(n)
	}
	if s, ok := bodyV.(lua.LString); ok {
		resp.body = string(s)
	}
	if t, ok := headersV.(*lua.LTable); ok {
		resp.headers = make(map[string]string)
		t.ForEach(func(k, v lua.LValue) {
			resp.headers[k.String()] = v.String()
		})
	}
	return resp, nil
}

// requestToLua builds the request table passed to a Lua handler.
func requestToLua(L *lua.LState, req *request) *lua.LTable {
	t := L.NewTable()
	t.RawSetString("method", lua.LString(req.method))
	t.RawSetString("path", lua.LString(req.path))
	t.RawSetString("body", lua.LString(req.body))
	t.RawSetString("query", stringMapToLua(L, req.query))
	t.RawSetString("headers", stringMapToLua(L, req.headers))
	return t
}

func stringMapToLua(L *lua.LState, m map[string]string) *lua.LTable {
	t := L.NewTable()
	for k, v := range m {
		t.RawSetString(k, lua.LString(v))
	}
	return t
}

// newLState creates a Lua state with a basic set of libraries.
// Full sandboxing (SkipOpenLibs + selective open) is applied in milestone 10.
func (r *Runner) newLState(ctx context.Context) *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	RegisterStdlib(L, r.scriptDir, r.root)
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
