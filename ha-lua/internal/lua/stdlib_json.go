package lua

import (
	lua "github.com/yuin/gopher-lua"
)

func registerJSON(L *lua.LState) {
	L.RegisterModule("json", jsonFuncs)
}

var jsonFuncs = map[string]lua.LGFunction{
	"encode": luaJSONEncode,
	"decode": luaJSONDecode,
}

func luaJSONEncode(L *lua.LState) int {
	v := L.CheckAny(1)
	b, err := luaMarshal(L, v)
	if err != nil {
		L.RaiseError("json.encode: %v", err)
		return 0
	}
	L.Push(lua.LString(b))
	return 1
}

func luaJSONDecode(L *lua.LState) int {
	s := L.CheckString(1)
	v, err := luaUnmarshal(L, []byte(s))
	if err != nil {
		L.RaiseError("json.decode: %v", err)
		return 0
	}
	L.Push(v)
	return 1
}
