package lua

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// newRequireState builds an LState with the restricted require pointed at a
// temp scripts dir whose lib/ contains the given modules (name → source).
func newRequireState(t *testing.T, libs map[string]string) *lua.LState {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range libs {
		if err := os.WriteFile(filepath.Join(libDir, name+".lua"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	L := lua.NewState()
	t.Cleanup(L.Close)
	installRestrictedRequire(L, dir)
	return L
}

func TestRequireCachesModules(t *testing.T) {
	L := newRequireState(t, map[string]string{
		"counter": `executed = (executed or 0) + 1; return { n = executed }`,
	})
	err := L.DoString(`
		local a = require("counter")
		local b = require("counter")
		assert(rawequal(a, b), "second require must return the cached table")
		assert(executed == 1, "module body ran " .. tostring(executed) .. " times")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequireCircularErrors(t *testing.T) {
	L := newRequireState(t, map[string]string{
		"a": `return require("b")`,
		"b": `return require("a")`,
	})
	err := L.DoString(`require("a")`)
	if err == nil || !strings.Contains(err.Error(), "circular require") {
		t.Fatalf("want circular require error, got %v", err)
	}
}

func TestRequireNoReturnYieldsTrue(t *testing.T) {
	L := newRequireState(t, map[string]string{
		"sideeffect": `did = true`,
	})
	err := L.DoString(`
		local v = require("sideeffect")
		assert(v == true, "module without a return value must yield true")
		assert(did == true, "module body must have run")
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequireOutsideLibFails(t *testing.T) {
	for _, path := range []string{"../secret", "/etc/passwd", "lib/../../x"} {
		L := newRequireState(t, nil)
		err := L.DoString(`require("` + path + `")`)
		if err == nil || !strings.Contains(err.Error(), "outside scripts/lib") {
			t.Errorf("require(%q): want path error, got %v", path, err)
		}
	}
}
