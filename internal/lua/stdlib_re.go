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
			// Move to front (LRU)
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

	if len(c.entries) >= reCacheLimit {
		// Remove last
		c.entries = c.entries[:reCacheLimit-1]
	}

	// Insert at front
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
	mod := L.RegisterModule("re", reFuncs)
	L.Push(mod)
}

var reFuncs = map[string]lua.LGFunction{
	"match":    luaREMatch,
	"find":     luaREFind,
	"find_all": luaREFindAll,
	"replace":  luaREReplace,
	"split":    luaRESplit,
}

func luaREMatch(L *lua.LState) int {
	pattern := L.CheckString(1)
	s := L.CheckString(2)
	cache := getRECache(L)
	re, err := cache.Get(pattern)
	if err != nil {
		L.RaiseError("re.match: %v", err)
		return 0
	}
	L.Push(lua.LBool(re.MatchString(s)))
	return 1
}

func luaREFind(L *lua.LState) int {
	pattern := L.CheckString(1)
	s := L.CheckString(2)
	cache := getRECache(L)
	re, err := cache.Get(pattern)
	if err != nil {
		L.RaiseError("re.find: %v", err)
		return 0
	}
	res := re.FindString(s)
	if res == "" && !re.MatchString(s) {
		L.Push(lua.LNil)
	} else {
		L.Push(lua.LString(res))
	}
	return 1
}

func luaREFindAll(L *lua.LState) int {
	pattern := L.CheckString(1)
	s := L.CheckString(2)
	cache := getRECache(L)
	re, err := cache.Get(pattern)
	if err != nil {
		L.RaiseError("re.find_all: %v", err)
		return 0
	}
	matches := re.FindAllString(s, -1)
	tbl := L.NewTable()
	for _, m := range matches {
		tbl.Append(lua.LString(m))
	}
	L.Push(tbl)
	return 1
}

func luaREReplace(L *lua.LState) int {
	pattern := L.CheckString(1)
	s := L.CheckString(2)
	repl := L.CheckString(3)
	cache := getRECache(L)
	re, err := cache.Get(pattern)
	if err != nil {
		L.RaiseError("re.replace: %v", err)
		return 0
	}
	L.Push(lua.LString(re.ReplaceAllString(s, repl)))
	return 1
}

func luaRESplit(L *lua.LState) int {
	pattern := L.CheckString(1)
	s := L.CheckString(2)
	cache := getRECache(L)
	re, err := cache.Get(pattern)
	if err != nil {
		L.RaiseError("re.split: %v", err)
		return 0
	}
	parts := re.Split(s, -1)
	tbl := L.NewTable()
	for _, p := range parts {
		tbl.Append(lua.LString(p))
	}
	L.Push(tbl)
	return 1
}
