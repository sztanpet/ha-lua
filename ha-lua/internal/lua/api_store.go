package lua

import (
	"context"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/store"
)

// kvStore is the method set shared by the per-script and global stores. The
// Lua bindings are identical; only the module name in error messages and the
// extra store.state() differ.
type kvStore interface {
	Get(ctx context.Context, key string) (any, error)
	Set(ctx context.Context, key string, value any) error
	Delete(ctx context.Context, key string) error
	GetAll(ctx context.Context) (map[string]any, error)
}

// registerStoreAPI installs the `store` and `global` modules on L.
func registerStoreAPI(L *lua.LState, kv *store.Store, global *store.GlobalStore) {
	storeTable := kvTable(L, "store", kv)
	L.SetField(storeTable, "state", L.NewFunction(func(L *lua.LState) int {
		defaults := L.OptTable(1, nil)
		proxy := newStateProxy(L, kv, defaults)
		L.Push(proxy)
		return 1
	}))
	L.SetGlobal("store", storeTable)

	L.SetGlobal("global", kvTable(L, "global", global))
}

// kvTable builds the get/set/delete/get_all module table over kv, with name
// prefixing error messages ("store.get: …" / "global.get: …").
func kvTable(L *lua.LState, name string, kv kvStore) *lua.LTable {
	t := L.NewTable()

	L.SetField(t, "get", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		v, err := kv.Get(L.Context(), key)
		if err != nil {
			L.RaiseError("%s.get: %v", name, err)
			return 0
		}
		if v == nil {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(anyToLua(L, v))
		return 1
	}))

	L.SetField(t, "set", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := L.CheckAny(2)
		goVal, err := luaToAny(L, val)
		if err != nil {
			L.RaiseError("%s.set: %v", name, err)
			return 0
		}
		if err := kv.Set(L.Context(), key, goVal); err != nil {
			L.RaiseError("%s.set: %v", name, err)
		}
		return 0
	}))

	L.SetField(t, "delete", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if err := kv.Delete(L.Context(), key); err != nil {
			L.RaiseError("%s.delete: %v", name, err)
		}
		return 0
	}))

	L.SetField(t, "get_all", L.NewFunction(func(L *lua.LState) int {
		all, err := kv.GetAll(L.Context())
		if err != nil {
			L.RaiseError("%s.get_all: %v", name, err)
			return 0
		}
		tbl := L.NewTable()
		for k, v := range all {
			tbl.RawSetString(k, anyToLua(L, v))
		}
		L.Push(tbl)
		return 1
	}))

	return t
}

// stateProxyData holds the in-memory cache for a store.state() proxy.
type stateProxyData struct {
	cache map[string]any
	kv    *store.Store
}

// newStateProxy creates a persistent-proxy table: reads from in-memory cache
// (preloaded from SQLite at construction), writes to both cache and SQLite.
func newStateProxy(L *lua.LState, kv *store.Store, defaults *lua.LTable) *lua.LTable {
	// Load all existing values under the script's context, like every other
	// binding — Background would outlive a script being stopped.
	existing, err := kv.GetAll(L.Context())
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
