package lua

import (
	"io/fs"
	"os"

	lua "github.com/yuin/gopher-lua"
)

// maxReadSize caps fs.read. A multi-MiB Lua string is already a smell; the
// assets this module exists to serve (HTML/CSS/JS for ha.serve) are KB-scale.
const maxReadSize = 8 << 20 // 8 MiB

// registerFS installs the read-only `fs` module. Every path is resolved through
// *os.Root, which confines access to the scripts directory and rejects symlink
// and ".." escapes at the syscall layer — unlike the hand-rolled containment in
// installRestrictedRequire. root may be nil (no scripts directory configured),
// in which case the read/list/stat calls return an error and exists returns
// false.
func registerFS(L *lua.LState, root *os.Root) {
	mod := L.RegisterModule("fs", map[string]lua.LGFunction{
		"read":   fsRead(root),
		"exists": fsExists(root),
		"list":   fsList(root),
		"stat":   fsStat(root),
	})
	L.Push(mod)
}

// fsErr pushes the (nil, message) failure pair shared by the fs functions,
// matching the http/json error convention.
func fsErr(L *lua.LState, msg string) int {
	L.Push(lua.LNil)
	L.Push(lua.LString(msg))
	return 2
}

// fsRead returns the whole file as a Lua string (binary-safe), or (nil, err).
func fsRead(root *os.Root) lua.LGFunction {
	return func(L *lua.LState) int {
		name := L.CheckString(1)
		if root == nil {
			return fsErr(L, "fs.read: filesystem unavailable")
		}
		info, err := root.Stat(name)
		if err != nil {
			return fsErr(L, err.Error())
		}
		if info.IsDir() {
			return fsErr(L, "fs.read: "+name+" is a directory")
		}
		if info.Size() > maxReadSize {
			return fsErr(L, "fs.read: "+name+" too large")
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return fsErr(L, err.Error())
		}
		L.Push(lua.LString(data))
		return 1
	}
}

// fsExists reports whether a path is reachable inside the root. It never raises;
// any error (missing, traversal, no root) reports false.
func fsExists(root *os.Root) lua.LGFunction {
	return func(L *lua.LState) int {
		name := L.CheckString(1)
		ok := false
		if root != nil {
			_, err := root.Stat(name)
			ok = err == nil
		}
		L.Push(lua.LBool(ok))
		return 1
	}
}

// fsList returns an array-table of the entry names in a directory (not
// recursive, no "." / ".."), or (nil, err). fs.list(".") lists the root.
func fsList(root *os.Root) lua.LGFunction {
	return func(L *lua.LState) int {
		name := L.CheckString(1)
		if root == nil {
			return fsErr(L, "fs.list: filesystem unavailable")
		}
		entries, err := fs.ReadDir(root.FS(), name)
		if err != nil {
			return fsErr(L, err.Error())
		}
		tbl := L.NewTable()
		for _, e := range entries {
			tbl.Append(lua.LString(e.Name()))
		}
		L.Push(tbl)
		return 1
	}
}

// fsStat returns {size, mtime (unix seconds), is_dir} for a path, or (nil, err).
func fsStat(root *os.Root) lua.LGFunction {
	return func(L *lua.LState) int {
		name := L.CheckString(1)
		if root == nil {
			return fsErr(L, "fs.stat: filesystem unavailable")
		}
		info, err := root.Stat(name)
		if err != nil {
			return fsErr(L, err.Error())
		}
		tbl := L.NewTable()
		tbl.RawSetString("size", lua.LNumber(info.Size()))
		tbl.RawSetString("mtime", lua.LNumber(info.ModTime().Unix()))
		tbl.RawSetString("is_dir", lua.LBool(info.IsDir()))
		L.Push(tbl)
		return 1
	}
}
