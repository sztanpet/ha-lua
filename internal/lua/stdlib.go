package lua

import (
	"path/filepath"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// installRestrictedRequire replaces the global `require` with one that only
// resolves paths inside scriptsDir/lib/.
func installRestrictedRequire(L *lua.LState, scriptsDir string) {
	libDir := filepath.Join(scriptsDir, "lib")

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

		if err := L.DoFile(luaPath); err != nil {
			L.RaiseError("require %q: %v", modName, err)
			return 0
		}
		return 1
	}))
}
