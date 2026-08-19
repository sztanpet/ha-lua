package lua

import (
	"encoding/json/v2"
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// maxTableDepth caps how deep luaToAny descends. A table that reaches itself
// ("t.self = t") would otherwise recurse until the goroutine stack overflows,
// and a Go stack overflow is a fatal error, not a panic: pcall cannot catch it,
// so one script's bad payload would take down the daemon and every other
// script's LState with it. Hitting the cap is an ordinary Lua error instead.
// The limit sits far above any legitimate payload — HA state attributes nest a
// handful of levels, not a hundred.
const maxTableDepth = 100

// luaToAny converts a Lua value to a Go value suitable for JSON marshaling.
func luaToAny(L *lua.LState, v lua.LValue) (any, error) {
	return luaToAnyDepth(L, v, 0)
}

// luaToAnyDepth is luaToAny with the current table nesting depth threaded
// through, so a cyclic table is rejected rather than crashing the process.
func luaToAnyDepth(L *lua.LState, v lua.LValue, depth int) (any, error) {
	switch val := v.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(val), nil
	case lua.LNumber:
		return float64(val), nil
	case lua.LString:
		return string(val), nil
	case *lua.LTable:
		if depth >= maxTableDepth {
			return nil, fmt.Errorf("table nested deeper than %d levels (cyclic?)", maxTableDepth)
		}
		return luaTableToAny(L, val, depth+1)
	default:
		return nil, fmt.Errorf("unsupported Lua type: %T", v)
	}
}

// luaTableToAny converts a Lua table to either a []any (array) or
// map[string]any (object). depth is this table's nesting level; see
// maxTableDepth.
func luaTableToAny(L *lua.LState, t *lua.LTable, depth int) (any, error) {
	// Detect array: integer keys 1..n with no holes and no string keys
	maxN := t.MaxN()
	if maxN > 0 {
		// Check if table is purely sequential
		isArray := true
		count := 0
		t.ForEach(func(k, _ lua.LValue) {
			count++
			if n, ok := k.(lua.LNumber); !ok || float64(n) != float64(int(n)) || int(n) < 1 || int(n) > maxN {
				isArray = false
			}
		})
		if isArray && count == maxN {
			arr := make([]any, maxN)
			for i := 1; i <= maxN; i++ {
				v, err := luaToAnyDepth(L, t.RawGetInt(i), depth)
				if err != nil {
					return nil, err
				}
				arr[i-1] = v
			}
			return arr, nil
		}
	}

	// Object
	obj := make(map[string]any)
	var retErr error
	t.ForEach(func(k, v lua.LValue) {
		if retErr != nil {
			return
		}
		key := lua.LVAsString(k)
		val, err := luaToAnyDepth(L, v, depth)
		if err != nil {
			retErr = err
			return
		}
		obj[key] = val
	})
	if retErr != nil {
		return nil, retErr
	}
	return obj, nil
}

// anyToLua converts a Go value (from JSON decode) to a Lua value.
func anyToLua(L *lua.LState, v any) lua.LValue {
	switch val := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(val)
	case float64:
		return lua.LNumber(val)
	case string:
		return lua.LString(val)
	case []any:
		t := L.NewTable()
		for i, elem := range val {
			t.RawSetInt(i+1, anyToLua(L, elem))
		}
		return t
	case map[string]any:
		t := L.NewTable()
		for k, elem := range val {
			t.RawSetString(k, anyToLua(L, elem))
		}
		return t
	default:
		return lua.LNil
	}
}

// luaMarshal marshals a Lua value to a JSON byte slice. Deterministic is
// required for stable output: json/v2 marshals map keys in random order by
// default, and scripts hash encoded payloads.
func luaMarshal(L *lua.LState, v lua.LValue) ([]byte, error) {
	goVal, err := luaToAny(L, v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(goVal, json.Deterministic(true))
}

// luaUnmarshal unmarshals JSON bytes into a Lua value.
func luaUnmarshal(L *lua.LState, data []byte) (lua.LValue, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return lua.LNil, err
	}
	return anyToLua(L, v), nil
}

// luaUnmarshalOrEmpty decodes raw JSON into a Lua value, falling back to an
// empty table for absent or malformed input — state/event consumers always
// get a table to index into, never nil.
func luaUnmarshalOrEmpty(L *lua.LState, raw []byte) lua.LValue {
	if len(raw) == 0 {
		return L.NewTable()
	}
	v, err := luaUnmarshal(L, raw)
	if err != nil {
		return L.NewTable()
	}
	return v
}
