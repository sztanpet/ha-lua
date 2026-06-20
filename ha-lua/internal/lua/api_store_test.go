package lua

import (
	"context"
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

func newTestLState(t testing.TB) (*lua.LState, *store.Store, *store.GlobalStore) {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	kv := store.New(writeDB, readDB, "test_script")
	global := store.NewGlobal(writeDB, readDB)

	L := lua.NewState()
	t.Cleanup(L.Close)
	L.SetContext(context.Background())
	registerStoreAPI(L, kv, global)
	return L, kv, global
}

func TestStoreGetSet(t *testing.T) {
	L, _, _ := newTestLState(t)

	if err := L.DoString(`store.set("key", 42)`); err != nil {
		t.Fatal(err)
	}
	if err := L.DoString(`_result = store.get("key")`); err != nil {
		t.Fatal(err)
	}
	v := L.GetGlobal("_result")
	if n, ok := v.(lua.LNumber); !ok || n != 42 {
		t.Errorf("want 42, got %v", v)
	}
}

func TestStoreStateProxy(t *testing.T) {
	L, _, _ := newTestLState(t)

	// First use: defaults applied
	if err := L.DoString(`
		local s = store.state({counter = 0, label = "hello"})
		_c1 = s.counter
		_l1 = s.label
		s.counter = s.counter + 1
		_c2 = s.counter
	`); err != nil {
		t.Fatalf("lua: %v", err)
	}

	if v := L.GetGlobal("_c1"); v != lua.LNumber(0) {
		t.Errorf("counter default: want 0, got %v", v)
	}
	if v := L.GetGlobal("_l1"); v != lua.LString("hello") {
		t.Errorf("label default: want hello, got %v", v)
	}
	if v := L.GetGlobal("_c2"); v != lua.LNumber(1) {
		t.Errorf("counter after inc: want 1, got %v", v)
	}
}

func TestStoreStateProxyPersists(t *testing.T) {
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	kv := store.New(writeDB, readDB, "persist_test")
	global := store.NewGlobal(writeDB, readDB)

	// Write via first LState
	L1 := lua.NewState()
	L1.SetContext(context.Background())
	registerStoreAPI(L1, kv, global)
	if err := L1.DoString(`store.state({val = 0}).val = 99`); err != nil {
		t.Fatal(err)
	}
	L1.Close()

	// Read via second LState — must see persisted value
	L2 := lua.NewState()
	t.Cleanup(L2.Close)
	L2.SetContext(context.Background())
	registerStoreAPI(L2, kv, global)
	if err := L2.DoString(`_v = store.state({val = 0}).val`); err != nil {
		t.Fatal(err)
	}
	if v := L2.GetGlobal("_v"); v != lua.LNumber(99) {
		t.Errorf("persisted value: want 99, got %v", v)
	}
}

func TestGlobalAPI(t *testing.T) {
	L, _, _ := newTestLState(t)

	if err := L.DoString(`
		global.set("x", "shared")
		_v = global.get("x")
	`); err != nil {
		t.Fatal(err)
	}
	if v := L.GetGlobal("_v"); v != lua.LString("shared") {
		t.Errorf("want shared, got %v", v)
	}
}

func BenchmarkStoreStateProxyWrite(b *testing.B) {
	writeDB, readDB := testutil.NewTestDB(b, nil)
	if err := state.Migrate(writeDB); err != nil {
		b.Fatal(err)
	}
	kv := store.New(writeDB, readDB, "bench")
	global := store.NewGlobal(writeDB, readDB)

	L := lua.NewState()
	b.Cleanup(L.Close)
	L.SetContext(context.Background())
	registerStoreAPI(L, kv, global)
	_ = L.DoString(`_proxy = store.state({n = 0})`)
	proxy := L.GetGlobal("_proxy")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		L.SetField(proxy.(*lua.LTable), "n", lua.LNumber(float64(i)))
	}
}
