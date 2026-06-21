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
	// onExceptionFn stores the registered exception handler for this script.
	onExceptionFn *lua.LFunction
	// stateChangeHandlers registered during load time.
	stateChangeHandlers []stateChangeHandler
	// eventHandlers registered during load time.
	eventHandlers []eventHandler
	// timerFns registered during load time or from callbacks.
	timerFns map[string]*lua.LFunction
	// timerIDs tracks load-time timers for PruneScript.
	timerIDs []string
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

	L.SetField(haTable, "every", L.NewFunction(func(L *lua.LState) int {
		if api.scheduler == nil {
			L.RaiseError("scheduler not available")
			return 0
		}
		spec := L.CheckString(1)
		fn := L.CheckFunction(2)
		seq := len(api.timerIDs) + 1
		id, err := api.scheduler.RegisterEvery(L.Context(), api.scriptID, spec, seq)
		if err != nil {
			L.RaiseError("every: %v", err)
			return 0
		}
		api.timerIDs = append(api.timerIDs, id)
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
		seq := len(api.timerIDs) + 1
		id, err := api.scheduler.RegisterAt(L.Context(), api.scriptID, spec, seq)
		if err != nil {
			L.RaiseError("at: %v", err)
			return 0
		}
		api.timerIDs = append(api.timerIDs, id)
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
	if len(s.Attributes) > 0 {
		attrs, err := luaUnmarshal(L, []byte(s.Attributes))
		if err != nil {
			tbl.RawSetString("attributes", L.NewTable())
		} else {
			tbl.RawSetString("attributes", attrs)
		}
	} else {
		tbl.RawSetString("attributes", L.NewTable())
	}
	tbl.RawSetString("last_changed", lua.LString(s.LastChanged))
	tbl.RawSetString("last_updated", lua.LString(s.LastUpdated))
	return tbl
}

// eventToLua converts an ha.Event to the Lua event table.
func eventToLua(L *lua.LState, ev ha.Event) *lua.LTable {
	tbl := L.NewTable()
	tbl.RawSetString("event_type", lua.LString(ev.Type))
	tbl.RawSetString("time_fired", lua.LString(ev.TimeFired))
	if len(ev.Data) > 0 {
		data, err := luaUnmarshal(L, []byte(ev.Data))
		if err != nil {
			tbl.RawSetString("data", L.NewTable())
		} else {
			tbl.RawSetString("data", data)
		}
	} else {
		tbl.RawSetString("data", L.NewTable())
	}
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
