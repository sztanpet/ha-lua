package store

import (
	"context"
	"testing"

	jjson "github.com/go-json-experiment/json"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

func newStore(t testing.TB, scriptID string) (*Store, *GlobalStore) {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	return New(writeDB, readDB, scriptID), NewGlobal(writeDB, readDB)
}

func TestGetSetDelete(t *testing.T) {
	s, _ := newStore(t, "script_a")
	ctx := context.Background()

	for _, tc := range []struct {
		key   string
		value any
	}{
		{"num", float64(42)},
		{"str", "hello"},
		{"bool", true},
		{"nested", map[string]any{"x": float64(1)}},
	} {
		if err := s.Set(ctx, tc.key, tc.value); err != nil {
			t.Fatalf("set %q: %v", tc.key, err)
		}
		got, err := s.Get(ctx, tc.key)
		if err != nil {
			t.Fatalf("get %q: %v", tc.key, err)
		}
		wantJSON, _ := jjson.Marshal(tc.value)
		gotJSON, _ := jjson.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("key %q: want %s, got %s", tc.key, wantJSON, gotJSON)
		}
	}

	if err := s.Delete(ctx, "num"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get(ctx, "num")
	if err != nil || v != nil {
		t.Errorf("after delete: want nil, got %v (err %v)", v, err)
	}
}

func TestScriptIsolation(t *testing.T) {
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	sa := New(writeDB, readDB, "script_a")
	sb := New(writeDB, readDB, "script_b")
	ctx := context.Background()

	_ = sa.Set(ctx, "key", "a_value")
	v, _ := sb.Get(ctx, "key")
	if v != nil {
		t.Errorf("script_b should not see script_a's key, got %v", v)
	}
}

func TestGetAll(t *testing.T) {
	s, _ := newStore(t, "test")
	ctx := context.Background()

	_ = s.Set(ctx, "a", float64(1))
	_ = s.Set(ctx, "b", "two")

	all, err := s.GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 keys, got %d", len(all))
	}
}

func TestGlobalStore(t *testing.T) {
	_, g := newStore(t, "script_a")
	ctx := context.Background()

	if err := g.Set(ctx, "shared", float64(99)); err != nil {
		t.Fatal(err)
	}
	v, err := g.Get(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if v != float64(99) {
		t.Errorf("global.get: want 99, got %v", v)
	}
}

func TestNilSet(t *testing.T) {
	s, _ := newStore(t, "s")
	ctx := context.Background()
	_ = s.Set(ctx, "k", "v")
	_ = s.Set(ctx, "k", nil) // nil = delete
	v, err := s.Get(ctx, "k")
	if err != nil || v != nil {
		t.Errorf("nil set: want nil, got %v (err %v)", v, err)
	}
}

func BenchmarkKVGet(b *testing.B) {
	s, _ := newStore(b, "bench")
	ctx := context.Background()
	_ = s.Set(ctx, "key", float64(42))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Get(ctx, "key")
	}
}

func BenchmarkKVSet(b *testing.B) {
	s, _ := newStore(b, "bench")
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Set(ctx, "key", float64(float64(i)))
	}
}
