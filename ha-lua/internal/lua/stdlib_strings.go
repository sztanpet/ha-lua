package lua

import (
	"strings"

	lua "github.com/yuin/gopher-lua"
)

func registerStrings(L *lua.LState) {
	L.RegisterModule("strings", stringsFuncs)
}

var stringsFuncs = map[string]lua.LGFunction{
	"contains":    luaStringsContains,
	"has_prefix":  luaStringsHasPrefix,
	"has_suffix":  luaStringsHasSuffix,
	"split":       luaStringsSplit,
	"join":        luaStringsJoin,
	"trim_space":  luaStringsTrimSpace,
	"trim":        luaStringsTrim,
	"replace_all": luaStringsReplaceAll,
	"count":       luaStringsCount,
	"fields":      luaStringsFields,
	"to_upper":    luaStringsToUpper,
	"to_lower":    luaStringsToLower,
}

func luaStringsContains(L *lua.LState) int {
	s := L.CheckString(1)
	substr := L.CheckString(2)
	L.Push(lua.LBool(strings.Contains(s, substr)))
	return 1
}

func luaStringsHasPrefix(L *lua.LState) int {
	s := L.CheckString(1)
	prefix := L.CheckString(2)
	L.Push(lua.LBool(strings.HasPrefix(s, prefix)))
	return 1
}

func luaStringsHasSuffix(L *lua.LState) int {
	s := L.CheckString(1)
	suffix := L.CheckString(2)
	L.Push(lua.LBool(strings.HasSuffix(s, suffix)))
	return 1
}

func luaStringsSplit(L *lua.LState) int {
	s := L.CheckString(1)
	sep := L.CheckString(2)
	var parts []string
	if sep == "" {
		// strings.Split would split into bytes; a Lua script wants characters.
		for _, r := range s {
			parts = append(parts, string(r))
		}
	} else {
		parts = strings.Split(s, sep)
	}
	return pushStringTable(L, parts)
}

func luaStringsJoin(L *lua.LState) int {
	tbl := L.CheckTable(1)
	sep := L.CheckString(2)
	var parts []string
	tbl.ForEach(func(_, v lua.LValue) {
		parts = append(parts, v.String())
	})
	L.Push(lua.LString(strings.Join(parts, sep)))
	return 1
}

func luaStringsTrimSpace(L *lua.LState) int {
	s := L.CheckString(1)
	L.Push(lua.LString(strings.TrimSpace(s)))
	return 1
}

func luaStringsTrim(L *lua.LState) int {
	s := L.CheckString(1)
	cutset := L.CheckString(2)
	L.Push(lua.LString(strings.Trim(s, cutset)))
	return 1
}

func luaStringsReplaceAll(L *lua.LState) int {
	s := L.CheckString(1)
	from := L.CheckString(2)
	to := L.CheckString(3)
	L.Push(lua.LString(strings.ReplaceAll(s, from, to)))
	return 1
}

func luaStringsCount(L *lua.LState) int {
	s := L.CheckString(1)
	substr := L.CheckString(2)
	L.Push(lua.LNumber(strings.Count(s, substr)))
	return 1
}

func luaStringsFields(L *lua.LState) int {
	return pushStringTable(L, strings.Fields(L.CheckString(1)))
}

func luaStringsToUpper(L *lua.LState) int {
	s := L.CheckString(1)
	L.Push(lua.LString(strings.ToUpper(s)))
	return 1
}

func luaStringsToLower(L *lua.LState) int {
	s := L.CheckString(1)
	L.Push(lua.LString(strings.ToLower(s)))
	return 1
}
