package lua

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-json-experiment/json/jsontext"
	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/ha"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
)

// haAPI holds the Go-side state needed by the ha.* Lua functions.
type haAPI struct {
	scriptID  string
	tracker   *state.Tracker
	scheduler *scheduler.Scheduler
	// callService sends a service call to HA; set by runner wiring.
	callService func(ctx context.Context, domain, service string, data jsontext.Value) error
	// fireEvent fires a HA event; set by runner wiring.
	fireEvent func(ctx context.Context, eventType string, data jsontext.Value) error
	// setState publishes an entity via the core REST API; set by runner wiring.
	setState func(ctx context.Context, entityID, state string, attrs jsontext.Value) (bool, error)
	// removeState removes a published entity; set by runner wiring.
	removeState func(ctx context.Context, entityID string) error
	// immediateEvents, set by ha.immediate_events() at load, opts the script out
	// of the default 100ms event coalescing so every state change is delivered
	// as it arrives.
	immediateEvents bool
	// onExceptionFn stores the registered exception handler for this script.
	onExceptionFn *lua.LFunction
	// stateChangeHandlers registered during load time.
	stateChangeHandlers []stateChangeHandler
	// eventHandlers registered during load time.
	eventHandlers []eventHandler
	// timerFns registered during load time or from callbacks.
	timerFns map[string]*lua.LFunction
	// keepIDs tracks every registered timer's ID so PruneScript does not
	// delete its row — including load-time ha.after rows, whose deletion
	// would silently lose the restart-orphan warning.
	keepIDs []string
	// timerSeq numbers ha.every/ha.at registrations. It is a dedicated
	// counter, NOT len(keepIDs): the seq is part of the stable timer ID
	// that carries last_run/next_run across reloads, so an ha.after call
	// between two ha.every calls must not renumber them.
	timerSeq int
	// routes registered via ha.serve during load time.
	routes []routeEntry
}

type stateChangeHandler struct {
	pattern string
	fn      *lua.LFunction
	initial bool
}

type routeEntry struct {
	method string
	prefix string
	fn     *lua.LFunction
}

// matchRoute returns the handler for the longest registered prefix of path
// under method, or nil. This is the authoritative lookup: the daemon Router's
// table is only a hint.
func (api *haAPI) matchRoute(method, path string) *lua.LFunction {
	var best *lua.LFunction
	bestLen := -1
	for _, e := range api.routes {
		if e.method != method {
			continue
		}
		if len(e.prefix) > bestLen && strings.HasPrefix(path, e.prefix) {
			best = e.fn
			bestLen = len(e.prefix)
		}
	}
	return best
}

// routeSpecs returns the (method, prefix) pairs for daemon Router registration.
func (api *haAPI) routeSpecs() []RouteSpec {
	out := make([]RouteSpec, 0, len(api.routes))
	for _, e := range api.routes {
		out = append(out, RouteSpec{Method: e.method, Prefix: e.prefix})
	}
	return out
}

type eventHandler struct {
	eventType string
	fn        *lua.LFunction
}

// registerHaAPI installs the `ha` module on L.
func (r *Runner) registerHaAPI(L *lua.LState, api *haAPI) {
	haTable := L.NewTable()

	// The script's own id (filename without extension). Exposed so libraries
	// like lib/card.lua can derive a default publish prefix without the runner
	// having to thread it through every call.
	L.SetField(haTable, "script_id", lua.LString(api.scriptID))

	L.SetField(haTable, "log", L.NewFunction(func(L *lua.LState) int {
		level := L.CheckString(1)
		msg := L.CheckString(2)
		switch level {
		case "debug":
			slog.Debug(msg, "script", api.scriptID)
		case "warn":
			slog.Warn(msg, "script", api.scriptID)
		case "error":
			slog.Error(msg, "script", api.scriptID)
		default:
			slog.Info(msg, "script", api.scriptID)
		}
		return 0
	}))

	L.SetField(haTable, "get_state", L.NewFunction(func(L *lua.LState) int {
		entityID := L.CheckString(1)
		s, err := api.tracker.GetState(L.Context(), entityID)
		if err != nil {
			L.RaiseError("get_state: %v", err)
			return 0
		}
		if s == nil {
			L.Push(lua.LNil)
			return 1
		}
		tbl := stateToLua(L, s)
		L.Push(tbl)
		return 1
	}))

	L.SetField(haTable, "get_entities", L.NewFunction(func(L *lua.LState) int {
		pattern := L.CheckString(1)
		states, err := api.tracker.GetEntities(L.Context(), pattern)
		if err != nil {
			L.RaiseError("get_entities: %v", err)
			return 0
		}
		tbl := L.NewTable()
		for i, s := range states {
			tbl.RawSetInt(i+1, stateToLua(L, &s))
		}
		L.Push(tbl)
		return 1
	}))

	L.SetField(haTable, "get_entity_ids", L.NewFunction(func(L *lua.LState) int {
		pattern := L.CheckString(1)
		ids, err := api.tracker.GetEntityIDs(L.Context(), pattern)
		if err != nil {
			L.RaiseError("get_entity_ids: %v", err)
			return 0
		}
		tbl := L.NewTable()
		for i, id := range ids {
			tbl.RawSetInt(i+1, lua.LString(id))
		}
		L.Push(tbl)
		return 1
	}))

	L.SetField(haTable, "get_history", L.NewFunction(func(L *lua.LState) int {
		entityID := L.CheckString(1)
		since := getTime(L, 2)
		limit := L.CheckInt(3)
		states, err := api.tracker.GetHistory(L.Context(), entityID, since, limit)
		if err != nil {
			L.RaiseError("get_history: %v", err)
			return 0
		}
		tbl := L.NewTable()
		for i, s := range states {
			tbl.RawSetInt(i+1, stateToLua(L, &s))
		}
		L.Push(tbl)
		return 1
	}))

	L.SetField(haTable, "call_service", L.NewFunction(func(L *lua.LState) int {
		if api.callService == nil {
			L.RaiseError("call_service not available")
			return 0
		}
		domain := L.CheckString(1)
		service := L.CheckString(2)
		data := jsontext.Value("{}")
		if L.GetTop() >= 3 {
			tbl := L.CheckTable(3)
			b, err := luaMarshal(L, tbl)
			if err != nil {
				L.RaiseError("call_service marshal: %v", err)
				return 0
			}
			data = jsontext.Value(b)
		}
		if err := api.callService(L.Context(), domain, service, data); err != nil {
			L.RaiseError("call_service: %v", err)
		}
		return 0
	}))

	L.SetField(haTable, "fire_event", L.NewFunction(func(L *lua.LState) int {
		if api.fireEvent == nil {
			L.RaiseError("fire_event not available")
			return 0
		}
		eventType := L.CheckString(1)
		data := jsontext.Value("{}")
		if L.GetTop() >= 2 {
			tbl := L.CheckTable(2)
			b, err := luaMarshal(L, tbl)
			if err != nil {
				L.RaiseError("fire_event marshal: %v", err)
				return 0
			}
			data = jsontext.Value(b)
		}
		if err := api.fireEvent(L.Context(), eventType, data); err != nil {
			L.RaiseError("fire_event: %v", err)
		}
		return 0
	}))

	// ha.set_state publishes (creates or updates) an entity via the core REST
	// API. Non-raising — returns created:bool|nil, err — because it rides the
	// per-minute control loop and a transient outage must not spam on_exception.
	L.SetField(haTable, "set_state", L.NewFunction(func(L *lua.LState) int {
		if api.setState == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("set_state not available"))
			return 2
		}
		entityID := L.CheckString(1)
		state := L.CheckString(2)
		var attrs jsontext.Value
		if L.GetTop() >= 3 && L.Get(3) != lua.LNil {
			tbl := L.CheckTable(3)
			b, err := luaMarshal(L, tbl)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString("set_state marshal: " + err.Error()))
				return 2
			}
			attrs = jsontext.Value(b)
		}
		created, err := api.setState(L.Context(), entityID, state, attrs)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LBool(created))
		return 1
	}))

	// ha.remove_state removes a published entity. Non-raising like set_state;
	// returns true|nil, err.
	L.SetField(haTable, "remove_state", L.NewFunction(func(L *lua.LState) int {
		if api.removeState == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("remove_state not available"))
			return 2
		}
		entityID := L.CheckString(1)
		if err := api.removeState(L.Context(), entityID); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LTrue)
		return 1
	}))

	// ha.on_command(handler) is sugar over on_event("ha_lua_command", …): it
	// keeps only commands addressed to this script (data.script == script id)
	// and calls handler(action, data) with the command's action and payload.
	// One inbound event type carries every card-driven command. Load-time only.
	L.SetField(haTable, "on_command", L.NewFunction(func(L *lua.LState) int {
		handler := L.CheckFunction(1)
		wrapper := L.NewFunction(func(L *lua.LState) int {
			data := L.OptTable(1, nil)
			if data == nil {
				return 0
			}
			if s := data.RawGetString("script"); s.Type() != lua.LTString || s.String() != api.scriptID {
				return 0
			}
			action := data.RawGetString("action")
			payload := data.RawGetString("data")
			L.Push(handler)
			L.Push(action)
			L.Push(payload)
			L.Call(2, 0)
			return 0
		})
		api.eventHandlers = append(api.eventHandlers, eventHandler{eventType: "ha_lua_command", fn: wrapper})
		return 0
	}))

	L.SetField(haTable, "every", L.NewFunction(func(L *lua.LState) int {
		if api.scheduler == nil {
			L.RaiseError("scheduler not available")
			return 0
		}
		spec := L.CheckString(1)
		fn := L.CheckFunction(2)
		api.timerSeq++
		id, err := api.scheduler.RegisterEvery(L.Context(), api.scriptID, spec, api.timerSeq)
		if err != nil {
			L.RaiseError("every: %v", err)
			return 0
		}
		api.keepIDs = append(api.keepIDs, id)
		api.timerFns[id] = fn
		return 0
	}))

	L.SetField(haTable, "at", L.NewFunction(func(L *lua.LState) int {
		if api.scheduler == nil {
			L.RaiseError("scheduler not available")
			return 0
		}
		spec := L.CheckString(1)
		fn := L.CheckFunction(2)
		api.timerSeq++
		id, err := api.scheduler.RegisterAt(L.Context(), api.scriptID, spec, api.timerSeq)
		if err != nil {
			L.RaiseError("at: %v", err)
			return 0
		}
		api.keepIDs = append(api.keepIDs, id)
		api.timerFns[id] = fn
		return 0
	}))

	L.SetField(haTable, "after", L.NewFunction(func(L *lua.LState) int {
		if api.scheduler == nil {
			L.RaiseError("scheduler not available")
			return 0
		}
		spec := L.CheckString(1)
		fn := L.CheckFunction(2)
		id, err := api.scheduler.RegisterAfter(L.Context(), api.scriptID, spec)
		if err != nil {
			L.RaiseError("after: %v", err)
			return 0
		}
		api.keepIDs = append(api.keepIDs, id)
		api.timerFns[id] = fn
		return 0
	}))

	// Registration functions — only valid at load time.
	L.SetField(haTable, "on_state_change", L.NewFunction(func(L *lua.LState) int {
		pattern := L.CheckString(1)
		fn := L.CheckFunction(2)
		// Match's error depends only on the pattern. Catch typos at load
		// time — dispatch silently ignores match errors, so a bad pattern
		// would otherwise just never fire.
		if _, err := filepath.Match(pattern, ""); err != nil {
			L.RaiseError("on_state_change: bad pattern %q: %v", pattern, err)
			return 0
		}
		opts := L.OptTable(3, nil)
		initial := false
		if opts != nil {
			if v := opts.RawGetString("initial"); v == lua.LTrue {
				initial = true
			}
		}
		api.stateChangeHandlers = append(api.stateChangeHandlers, stateChangeHandler{
			pattern: pattern,
			fn:      fn,
			initial: initial,
		})
		return 0
	}))

	L.SetField(haTable, "on_event", L.NewFunction(func(L *lua.LState) int {
		eventType := L.CheckString(1)
		fn := L.CheckFunction(2)
		api.eventHandlers = append(api.eventHandlers, eventHandler{
			eventType: eventType,
			fn:        fn,
		})
		return 0
	}))

	// ha.immediate_events() opts this script out of the default 100ms event
	// coalescing: every state change is delivered as it arrives (no per-entity
	// collapse, no batching delay). Call at load time. Use it only when a handler
	// must see every transition; the default is cheaper and avoids dropped events.
	L.SetField(haTable, "immediate_events", L.NewFunction(func(L *lua.LState) int {
		api.immediateEvents = true
		return 0
	}))

	// ha.serve registers an HTTP handler for a method + path prefix. Load-time
	// only. The handler receives a request table {method, path, query, headers,
	// body} and returns status:int[, body:string[, headers:table]].
	L.SetField(haTable, "serve", L.NewFunction(func(L *lua.LState) int {
		method := strings.ToUpper(L.CheckString(1))
		prefix := L.CheckString(2)
		fn := L.CheckFunction(3)
		if !strings.HasPrefix(prefix, "/") {
			L.RaiseError("serve: path prefix must start with '/', got %q", prefix)
			return 0
		}
		api.routes = append(api.routes, routeEntry{method: method, prefix: prefix, fn: fn})
		return 0
	}))

	L.SetField(haTable, "on_exception", L.NewFunction(func(L *lua.LState) int {
		fn := L.CheckFunction(1)
		api.onExceptionFn = fn
		return 0
	}))

	// ha.exceptions built-in handlers
	exceptionsTable := L.NewTable()
	registerExceptionHandlers(L, exceptionsTable)
	L.SetField(haTable, "exceptions", exceptionsTable)

	L.SetGlobal("ha", haTable)
}

// stateToLua converts a StateData to a Lua table.
func stateToLua(L *lua.LState, s *ha.StateData) *lua.LTable {
	tbl := L.NewTable()
	tbl.RawSetString("entity_id", lua.LString(s.EntityID))
	tbl.RawSetString("state", lua.LString(s.State))
	tbl.RawSetString("attributes", luaUnmarshalOrEmpty(L, []byte(s.Attributes)))
	tbl.RawSetString("last_changed", lua.LString(s.LastChanged))
	tbl.RawSetString("last_updated", lua.LString(s.LastUpdated))
	return tbl
}

// eventToLua converts an ha.Event to the Lua event table.
func eventToLua(L *lua.LState, ev ha.Event) *lua.LTable {
	tbl := L.NewTable()
	tbl.RawSetString("event_type", lua.LString(ev.Type))
	tbl.RawSetString("time_fired", lua.LString(ev.TimeFired))
	tbl.RawSetString("data", luaUnmarshalOrEmpty(L, []byte(ev.Data)))
	return tbl
}

// dispatchException calls the registered on_exception handler (if any) or logs.
func dispatchException(L *lua.LState, api *haAPI, errMsg, traceback, callbackName string, eventTbl lua.LValue) {
	info := L.NewTable()
	info.RawSetString("script_id", lua.LString(api.scriptID))
	info.RawSetString("error", lua.LString(errMsg))
	info.RawSetString("traceback", lua.LString(traceback))
	info.RawSetString("callback", lua.LString(callbackName))
	info.RawSetString("timestamp", lua.LString(time.Now().UTC().Format(time.RFC3339)))
	if eventTbl != nil {
		info.RawSetString("event", eventTbl)
	} else {
		info.RawSetString("event", lua.LNil)
	}

	if api.onExceptionFn != nil {
		if err := L.CallByParam(lua.P{
			Fn:      api.onExceptionFn,
			NRet:    0,
			Protect: true,
		}, info); err != nil {
			slog.Error("ha: exception handler itself errored",
				"script", api.scriptID, "err", err)
		}
		return
	}
	slog.Error("ha: unhandled script error",
		"script", api.scriptID,
		"callback", callbackName,
		"error", errMsg,
		"traceback", traceback)
}

// luaErrParts splits a protected-call error into message and stack trace.
// ApiError.Error() glues them together, which would make info.traceback a copy
// of info.error.
func luaErrParts(err error) (msg, traceback string) {
	msg = err.Error()
	var apiErr *lua.ApiError
	if errors.As(err, &apiErr) {
		msg = apiErr.Object.String()
		traceback = apiErr.StackTrace
	}
	return msg, traceback
}

// callProtected calls fn with args in a protected call, dispatching any error
// to the exception handler.
func callProtected(L *lua.LState, api *haAPI, callbackName string, eventTbl lua.LValue, fn *lua.LFunction, args ...lua.LValue) {
	params := lua.P{Fn: fn, NRet: 0, Protect: true}
	if err := L.CallByParam(params, args...); err != nil {
		errMsg, traceback := luaErrParts(err)
		dispatchException(L, api, errMsg, traceback, callbackName, eventTbl)
	}
}
