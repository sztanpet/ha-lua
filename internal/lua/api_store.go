package lua

import (
	"context"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/store"
)

// registerStoreAPI installs the `store` and `global` modules on L.
func registerStoreAPI(L *lua.LState, kv *store.Store, global *store.GlobalStore) {
	storeTable := L.NewTable()

	L.SetField(storeTable, "get", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		v, err := kv.Get(L.Context(), key)
		if err != nil {
			L.RaiseError("store.get: %v", err)
			return 0
		}
		if v == nil {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(anyToLua(L, v))
		return 1
	}))

	L.SetField(storeTable, "set", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := L.CheckAny(2)
		goVal, err := luaToAny(L, val)
		if err != nil {
			L.RaiseError("store.set: %v", err)
			return 0
		}
		if err := kv.Set(L.Context(), key, goVal); err != nil {
			L.RaiseError("store.set: %v", err)
		}
		return 0
	}))

	L.SetField(storeTable, "delete", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if err := kv.Delete(L.Context(), key); err != nil {
			L.RaiseError("store.delete: %v", err)
		}
		return 0
	}))

	L.SetField(storeTable, "get_all", L.NewFunction(func(L *lua.LState) int {
		all, err := kv.GetAll(L.Context())
		if err != nil {
			L.RaiseError("store.get_all: %v", err)
			return 0
		}
		tbl := L.NewTable()
		for k, v := range all {
			tbl.RawSetString(k, anyToLua(L, v))
		}
		L.Push(tbl)
		return 1
	}))

	L.SetField(storeTable, "state", L.NewFunction(func(L *lua.LState) int {
		defaults := L.OptTable(1, nil)
		proxy := newStateProxy(L, kv, defaults)
		L.Push(proxy)
		return 1
	}))

	L.SetGlobal("store", storeTable)

	// global module
	globalTable := L.NewTable()

	L.SetField(globalTable, "get", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		v, err := global.Get(L.Context(), key)
		if err != nil {
			L.RaiseError("global.get: %v", err)
			return 0
		}
		if v == nil {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(anyToLua(L, v))
		return 1
	}))

	L.SetField(globalTable, "set", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := L.CheckAny(2)
		goVal, err := luaToAny(L, val)
		if err != nil {
			L.RaiseError("global.set: %v", err)
			return 0
		}
		if err := global.Set(L.Context(), key, goVal); err != nil {
			L.RaiseError("global.set: %v", err)
		}
		return 0
	}))

	L.SetField(globalTable, "delete", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if err := global.Delete(L.Context(), key); err != nil {
			L.RaiseError("global.delete: %v", err)
		}
		return 0
	}))

	L.SetField(globalTable, "get_all", L.NewFunction(func(L *lua.LState) int {
		all, err := global.GetAll(L.Context())
		if err != nil {
			L.RaiseError("global.get_all: %v", err)
			return 0
		}
		tbl := L.NewTable()
		for k, v := range all {
			tbl.RawSetString(k, anyToLua(L, v))
		}
		L.Push(tbl)
		return 1
	}))

	L.SetGlobal("global", globalTable)
}

// stateProxyData holds the in-memory cache for a store.state() proxy.
type stateProxyData struct {
	cache map[string]any
	kv    *store.Store
}

// newStateProxy creates a persistent-proxy table: reads from in-memory cache
// (preloaded from SQLite at construction), writes to both cache and SQLite.
func newStateProxy(L *lua.LState, kv *store.Store, defaults *lua.LTable) *lua.LTable {
	// Load all existing values
	existing, err := kv.GetAll(context.Background())
	if err != nil {
		L.RaiseError("store.state load: %v", err)
		return L.NewTable()
	}

	// Seed cache from existing + defaults
	cache := make(map[string]any)
	if defaults != nil {
		defaults.ForEach(func(k, v lua.LValue) {
			key := lua.LVAsString(k)
			goVal, _ := luaToAny(L, v)
			cache[key] = goVal
		})
	}
	for k, v := range existing {
		cache[k] = v
	}

	data := &stateProxyData{cache: cache, kv: kv}

	proxy := L.NewTable()
	mt := L.NewTable()

	L.SetField(mt, "__index", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(2)
		if v, ok := data.cache[key]; ok {
			L.Push(anyToLua(L, v))
		} else {
			L.Push(lua.LNil)
		}
		return 1
	}))

	L.SetField(mt, "__newindex", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(2)
		val := L.CheckAny(3)
		goVal, err := luaToAny(L, val)
		if err != nil {
			L.RaiseError("store.state set %q: %v", key, err)
			return 0
		}
		data.cache[key] = goVal
		if err := kv.Set(L.Context(), key, goVal); err != nil {
			L.RaiseError("store.state persist %q: %v", key, err)
		}
		return 0
	}))

	L.SetMetatable(proxy, mt)
	return proxy
}
