package lua

import (
	"os"
	"path/filepath"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// RegisterStdlib applies full sandboxing (SkipOpenLibs + selective open) and
// registers all additional modules (strings, time, json, re, http, crypto, fs)
// and math augmentations. root is the os.Root backing the read-only fs module;
// it may be nil (fs calls then error).
func RegisterStdlib(L *lua.LState, scriptsDir string, root *os.Root) {
	// 1. Selective open of standard libraries
	for _, lib := range []struct {
		name string
		open lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
		{lua.OsLibName, lua.OpenOs},
		{lua.CoroutineLibName, lua.OpenCoroutine},
	} {
		L.Push(L.NewFunction(lib.open))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}

	// 2. Sandboxing: Remove/Nil dangerous functions
	// Removed from _G
	L.SetGlobal("load", lua.LNil)
	L.SetGlobal("loadstring", lua.LNil)
	L.SetGlobal("loadfile", lua.LNil)
	L.SetGlobal("dofile", lua.LNil)
	L.SetGlobal("module", lua.LNil)
	L.SetGlobal("package", lua.LNil)

	// Restricted os module
	if osMod, ok := L.GetGlobal("os").(*lua.LTable); ok {
		allowed := map[string]bool{
			"clock":    true,
			"date":     true,
			"difftime": true,
			"time":     true,
		}
		osMod.ForEach(func(k, v lua.LValue) {
			if !allowed[k.String()] {
				osMod.RawSet(k, lua.LNil)
			}
		})
	}

	// 3. Install restricted require
	installRestrictedRequire(L, scriptsDir, root)

	// 4. Register custom modules
	registerMath(L)
	registerStrings(L)
	registerTime(L)
	registerJSON(L)
	registerRE(L)
	registerHTTP(L)
	registerCrypto(L)
	registerFS(L, root)
}

func installRestrictedRequire(L *lua.LState, scriptsDir string, root *os.Root) {
	libDir := filepath.Join(scriptsDir, "lib")
	loaded := make(map[string]lua.LValue)
	loading := make(map[string]bool)

	L.SetGlobal("require", L.NewFunction(func(L *lua.LState) int {
		modName := L.CheckString(1)
		clean := filepath.Clean(modName)
		// Cheap lexical guard: keep require's "lib/ only" contract and the
		// stable error message tests assert on. os.Root below handles the
		// rest (symlinks, races) at the syscall layer.
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			L.RaiseError("require: path outside scripts/lib not allowed: %q", modName)
			return 0
		}
		if root == nil {
			L.RaiseError("require %q: filesystem unavailable", modName)
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

		// Resolve through the shared *os.Root: it confines the open to the
		// scripts directory and rejects symlink escapes that the old lexical
		// filepath.Abs + HasPrefix check could not see through. The path is
		// relative to the root; the chunk name keeps the lib path for
		// readable tracebacks.
		file, err := root.Open(filepath.Join("lib", clean+".lua"))
		if err != nil {
			L.RaiseError("require %q: %v", modName, err)
			return 0
		}
		fn, err := L.Load(file, filepath.Join(libDir, clean+".lua"))
		_ = file.Close()
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
