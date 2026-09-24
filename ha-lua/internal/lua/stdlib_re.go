package lua

import (
	"regexp"

	lua "github.com/yuin/gopher-lua"
)

const reCacheRegistryKey = "re_cache"
const reCacheLimit = 256

type reCacheEntry struct {
	pattern string
	re      *regexp.Regexp
}

type reCache struct {
	entries []reCacheEntry
}

func (c *reCache) Get(pattern string) (*regexp.Regexp, error) {
	for i, entry := range c.entries {
		if entry.pattern == pattern {
			if i > 0 {
				copy(c.entries[1:i+1], c.entries[0:i])
				c.entries[0] = entry
			}
			return entry.re, nil
		}
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	// Newest at the front, so the least recently used falls off the end.
	if len(c.entries) >= reCacheLimit {
		c.entries = c.entries[:reCacheLimit-1]
	}
	c.entries = append([]reCacheEntry{{pattern: pattern, re: re}}, c.entries...)
	return re, nil
}

func getRECache(L *lua.LState) *reCache {
	key := lua.LString(reCacheRegistryKey)
	registry := L.Get(lua.RegistryIndex).(*lua.LTable)
	val := registry.RawGet(key)
	if val == lua.LNil {
		cache := &reCache{}
		ud := L.NewUserData()
		ud.Value = cache
		registry.RawSet(key, ud)
		return cache
	}
	return val.(*lua.LUserData).Value.(*reCache)
}

func registerRE(L *lua.LState) {
	L.RegisterModule("re", reFuncs)
}

var reFuncs = map[string]lua.LGFunction{
	"match":    luaREMatch,
	"find":     luaREFind,
	"find_all": luaREFindAll,
	"replace":  luaREReplace,
	"split":    luaRESplit,
}

// compiled returns the compiled pattern from argument 1, raising re.<name> if
// it does not compile. RaiseError unwinds, so a nil return never reaches the
// caller.
func compiled(L *lua.LState, name string) *regexp.Regexp {
	pattern := L.CheckString(1)
	re, err := getRECache(L).Get(pattern)
	if err != nil {
		L.RaiseError("re.%s: %v", name, err)
		return nil
	}
	return re
}

// pushStringTable pushes values as an array-table.
func pushStringTable(L *lua.LState, values []string) int {
	tbl := L.NewTable()
	for _, v := range values {
		tbl.Append(lua.LString(v))
	}
	L.Push(tbl)
	return 1
}

func luaREMatch(L *lua.LState) int {
	re := compiled(L, "match")
	L.Push(lua.LBool(re.MatchString(L.CheckString(2))))
	return 1
}

func luaREFind(L *lua.LState) int {
	re := compiled(L, "find")
	s := L.CheckString(2)
	// By index, not FindString: an empty match is still a match, and "" alone
	// cannot say whether the pattern matched.
	loc := re.FindStringIndex(s)
	if loc == nil {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(lua.LString(s[loc[0]:loc[1]]))
	return 1
}

func luaREFindAll(L *lua.LState) int {
	re := compiled(L, "find_all")
	return pushStringTable(L, re.FindAllString(L.CheckString(2), -1))
}

func luaREReplace(L *lua.LState) int {
	re := compiled(L, "replace")
	s := L.CheckString(2)
	repl := L.CheckString(3)
	L.Push(lua.LString(re.ReplaceAllString(s, repl)))
	return 1
}

func luaRESplit(L *lua.LState) int {
	re := compiled(L, "split")
	return pushStringTable(L, re.Split(L.CheckString(2), -1))
}
