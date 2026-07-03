package lua

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// newFSState builds a sandboxed LState whose fs module is rooted at dir.
func newFSState(t *testing.T, dir string) *lua.LState {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	t.Cleanup(L.Close)
	L.SetContext(context.Background())
	RegisterStdlib(L, dir, root)
	return L
}

func TestFSRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	L := newFSState(t, dir)

	if err := L.DoString(`
		local data, err = fs.read("page.html")
		assert(err == nil, "unexpected err: "..tostring(err))
		assert(data == "<h1>hi</h1>", "got: "..tostring(data))

		-- missing file -> (nil, err)
		local d2, e2 = fs.read("nope.html")
		assert(d2 == nil and e2 ~= nil, "missing file should error")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSReadRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	L := newFSState(t, dir)
	if err := L.DoString(`
		local d, e = fs.read("sub")
		assert(d == nil and e ~= nil, "reading a directory should error")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSReadTooLarge(t *testing.T) {
	dir := t.TempDir()
	// Truncate to past the cap: a sparse file Stat reports as oversized, so the
	// size guard trips before any bytes are read.
	f, err := os.Create(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxReadSize + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	L := newFSState(t, dir)
	if err := L.DoString(`
		local d, e = fs.read("big.bin")
		assert(d == nil and e ~= nil, "oversized file should error")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSReadRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	// A secret one level above the root.
	if err := os.WriteFile(filepath.Join(dir, "..", "secret.txt"), []byte("s3cr3t"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, "..", "secret.txt")) })

	// A symlink inside the root pointing outside it.
	if err := os.Symlink(filepath.Join(dir, "..", "secret.txt"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	L := newFSState(t, dir)
	for _, path := range []string{"../secret.txt", "/etc/hostname", "link"} {
		L.SetGlobal("p", lua.LString(path))
		if err := L.DoString(`
			local d, e = fs.read(p)
			assert(d == nil and e ~= nil, "escape via "..p.." should error")
		`); err != nil {
			t.Fatalf("path %q: %v", path, err)
		}
	}
}

func TestFSExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	L := newFSState(t, dir)
	if err := L.DoString(`
		assert(fs.exists("a.txt") == true, "a.txt should exist")
		assert(fs.exists("missing") == false, "missing should not exist")
		assert(fs.exists("../etc") == false, "escape should not exist")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSList(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.lua", "b.lua"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	L := newFSState(t, dir)
	if err := L.DoString(`
		local names, err = fs.list(".")
		assert(err == nil, "list err: "..tostring(err))
		local seen = {}
		for _, n in ipairs(names) do seen[n] = true end
		assert(seen["a.lua"] and seen["b.lua"] and seen["lib"], "missing entries")

		-- listing a file is an error
		local d, e = fs.list("a.lua")
		assert(d == nil and e ~= nil, "listing a file should error")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSStat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	L := newFSState(t, dir)
	if err := L.DoString(`
		local info = fs.stat("f.txt")
		assert(info ~= nil, "stat f.txt")
		assert(info.size == 5, "size: "..tostring(info.size))
		assert(info.is_dir == false, "f.txt not a dir")
		assert(info.mtime > 0, "mtime set")

		assert(fs.stat("d").is_dir == true, "d is a dir")

		local i, e = fs.stat("nope")
		assert(i == nil and e ~= nil, "stat missing should error")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSWrite(t *testing.T) {
	dir := t.TempDir()
	L := newFSState(t, dir)
	if err := L.DoString(`
		assert(fs.write("out.txt", "hello"), "write failed")
		assert(fs.read("out.txt") == "hello", "read back")

		-- write truncates, not appends
		assert(fs.write("out.txt", "bye"), "rewrite failed")
		assert(fs.read("out.txt") == "bye", "truncate on rewrite")

		-- missing parent dir is an error, not auto-created
		local ok, err = fs.write("no/such/dir.txt", "x")
		assert(ok == nil and err ~= nil, "missing parent should error")

		-- escapes rejected
		local ok2, err2 = fs.write("../escape.txt", "x")
		assert(ok2 == nil and err2 ~= nil, "escape should error")
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escape.txt")); err == nil {
		t.Fatal("escape.txt was created outside the root")
	}
}

func TestFSAppend(t *testing.T) {
	L := newFSState(t, t.TempDir())
	if err := L.DoString(`
		-- append creates the file
		assert(fs.append("log.txt", "a"), "first append")
		assert(fs.append("log.txt", "b"), "second append")
		assert(fs.read("log.txt") == "ab", "appends accumulate")

		local ok, err = fs.append("../escape.txt", "x")
		assert(ok == nil and err ~= nil, "escape should error")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSMkdir(t *testing.T) {
	L := newFSState(t, t.TempDir())
	if err := L.DoString(`
		assert(fs.mkdir("a/b/c"), "nested mkdir")
		assert(fs.stat("a/b/c").is_dir == true, "created as dir")
		assert(fs.mkdir("a/b/c"), "existing dir is not an error")
		assert(fs.write("a/b/c/f.txt", "x"), "write inside new dir")

		local ok, err = fs.mkdir("../outside")
		assert(ok == nil and err ~= nil, "escape should error")
	`); err != nil {
		t.Fatal(err)
	}
}

func TestFSRemove(t *testing.T) {
	L := newFSState(t, t.TempDir())
	if err := L.DoString(`
		assert(fs.write("f.txt", "x"), "setup write")
		assert(fs.remove("f.txt"), "remove file")
		assert(fs.exists("f.txt") == false, "file gone")

		assert(fs.mkdir("d"), "setup dir")
		assert(fs.remove("d"), "remove empty dir")

		-- non-empty dir is an error (not recursive)
		assert(fs.mkdir("full"), "setup full dir")
		assert(fs.write("full/f.txt", "x"), "setup file in dir")
		local ok, err = fs.remove("full")
		assert(ok == nil and err ~= nil, "non-empty dir should error")

		local ok2, err2 = fs.remove("missing")
		assert(ok2 == nil and err2 ~= nil, "missing path should error")
	`); err != nil {
		t.Fatal(err)
	}
}

// TestFSNilRoot verifies the module degrades gracefully when no root is wired
// (the convention tests pass nil): calls error rather than panic.
func TestFSNilRoot(t *testing.T) {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	t.Cleanup(L.Close)
	L.SetContext(context.Background())
	RegisterStdlib(L, t.TempDir(), nil)
	if err := L.DoString(`
		local d, e = fs.read("x")
		assert(d == nil and e ~= nil, "read without root should error")
		assert(fs.exists("x") == false, "exists without root is false")
		local l, le = fs.list(".")
		assert(l == nil and le ~= nil, "list without root should error")
		local w, we = fs.write("x", "y")
		assert(w == nil and we ~= nil, "write without root should error")
		local a, ae = fs.append("x", "y")
		assert(a == nil and ae ~= nil, "append without root should error")
		local m, me = fs.mkdir("x")
		assert(m == nil and me ~= nil, "mkdir without root should error")
		local r, re = fs.remove("x")
		assert(r == nil and re ~= nil, "remove without root should error")
	`); err != nil {
		t.Fatal(err)
	}
}
