package lua

import (
	"fmt"

	"github.com/go-json-experiment/json"
	lua "github.com/yuin/gopher-lua"
)

// luaToAny converts a Lua value to a Go value suitable for JSON marshaling.
func luaToAny(L *lua.LState, v lua.LValue) (any, error) {
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
		return luaTableToAny(L, val)
	default:
		return nil, fmt.Errorf("unsupported Lua type: %T", v)
	}
}

// luaTableToAny converts a Lua table to either a []any (array) or map[string]any (object).
func luaTableToAny(L *lua.LState, t *lua.LTable) (any, error) {
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
				v, err := luaToAny(L, t.RawGetInt(i))
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
		val, err := luaToAny(L, v)
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
