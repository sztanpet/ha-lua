package lua

import (
	"path/filepath"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// installRestrictedRequire replaces the global `require` with one that only
// resolves paths inside scriptsDir/lib/. It implements real require
// semantics: each module runs once per LState and its return value is
// cached; a circular require chain raises an error instead of recursing
// until the stack dies.
func installRestrictedRequire(L *lua.LState, scriptsDir string) {
	libDir := filepath.Join(scriptsDir, "lib")
	loaded := make(map[string]lua.LValue)
	loading := make(map[string]bool)

	L.SetGlobal("require", L.NewFunction(func(L *lua.LState) int {
		modName := L.CheckString(1)
		clean := filepath.Clean(modName)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			L.RaiseError("require: path outside scripts/lib not allowed: %q", modName)
			return 0
		}
		luaPath := filepath.Join(libDir, clean+".lua")
		// Double-check the resolved path is still under libDir
		absLib, _ := filepath.Abs(libDir)
		absPath, _ := filepath.Abs(luaPath)
		if !strings.HasPrefix(absPath, absLib+string(filepath.Separator)) &&
			absPath != absLib {
			L.RaiseError("require: path outside scripts/lib not allowed: %q", modName)
			return 0
		}

		if v, ok := loaded[clean]; ok {
			L.Push(v)
			return 1
		}
		if loading[clean] {
			L.RaiseError("circular require: %q", modName)
			return 0
		}
		loading[clean] = true
		defer delete(loading, clean)

		fn, err := L.LoadFile(luaPath)
		if err != nil {
			L.RaiseError("require %q: %v", modName, err)
			return 0
		}
		L.Push(fn)
		if err := L.PCall(0, 1, nil); err != nil {
			L.RaiseError("require %q: %v", modName, err)
			return 0
		}
		ret := L.Get(-1)
		L.Pop(1)
		if ret == lua.LNil {
			// Lua convention: a module that returns nothing is recorded as true.
			ret = lua.LTrue
		}
		loaded[clean] = ret
		L.Push(ret)
		return 1
	}))
}
