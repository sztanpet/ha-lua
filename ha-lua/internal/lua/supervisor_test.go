package lua

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

func newSupervisor(t testing.TB, scriptDir string) (*Supervisor, *Registry, *store.GlobalStore) {
	t.Helper()
	writeDB, readDB := testutil.NewTestDB(t, nil)
	if err := state.Migrate(writeDB); err != nil {
		t.Fatal(err)
	}
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)
	sup := NewSupervisor(reg, scriptDir, Deps{
		Tracker:   state.New(writeDB, readDB),
		Scheduler: sched,
		Global:    global,
		Root:      openTestRoot(t, scriptDir),
		NewKV: func(id string) *store.Store {
			return store.New(writeDB, readDB, id)
		},
	})
	return sup, reg, global
}

func writeScript(t testing.TB, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// globalString polls the global KV until key has the wanted value or the
// deadline expires. Script loading is asynchronous.
func globalString(t testing.TB, global *store.GlobalStore, key, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got any
	for time.Now().Before(deadline) {
		var err error
		got, err = global.Get(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := got.(string); ok && s == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("global %q = %v, want %q", key, got, want)
}

func TestSupervisorLoadAll(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "one.lua", `global.set("one", "loaded")`)
	writeScript(t, dir, "two.lua", `global.set("two", "loaded")`)
	writeScript(t, dir, "ignored.txt", `not lua`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup, reg, global := newSupervisor(t, dir)
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); sup.Wait() }() // cancel first: Wait blocks until runners exit

	globalString(t, global, "one", "loaded")
	globalString(t, global, "two", "loaded")
	if reg.Get("one") == nil || reg.Get("two") == nil {
		t.Error("runners not registered")
	}
	if reg.Get("ignored") != nil {
		t.Error("non-lua file got a runner")
	}
}

// TestSupervisorLoadAllNoRoot: script enumeration goes through the shared
// os.Root; a Supervisor wired without one must fail LoadAll loudly instead of
// silently loading nothing.
func TestSupervisorLoadAllNoRoot(t *testing.T) {
	sup := NewSupervisor(NewRegistry(), t.TempDir(), Deps{})
	if err := sup.LoadAll(context.Background()); err == nil {
		t.Fatal("LoadAll with nil root should error")
	}
}

func TestSupervisorReload(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "s.lua", `global.set("ver", "v1")`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup, reg, global := newSupervisor(t, dir)
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); sup.Wait() }() // cancel first: Wait blocks until runners exit

	globalString(t, global, "ver", "v1")
	first := reg.Get("s")

	writeScript(t, dir, "s.lua", `global.set("ver", "v2")`)
	sup.Reload(ctx, "s")

	globalString(t, global, "ver", "v2")
	second := reg.Get("s")
	if second == nil || second == first {
		t.Error("reload must replace the runner with a fresh one")
	}
}

func TestSupervisorStop(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "s.lua", `global.set("ver", "v1")`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup, reg, global := newSupervisor(t, dir)
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	globalString(t, global, "ver", "v1")

	sup.StopScript("s")
	if reg.Get("s") != nil {
		t.Error("stopped script still in registry")
	}
	sup.StopScript("s") // second stop must be a no-op
	sup.Wait()          // goroutine must have exited
}

func TestSupervisorReloadStartsNewScript(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup, reg, global := newSupervisor(t, dir)
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); sup.Wait() }() // cancel first: Wait blocks until runners exit

	// File created after startup: Reload acts as Start.
	writeScript(t, dir, "fresh.lua", `global.set("fresh", "yes")`)
	sup.Reload(ctx, "fresh")

	globalString(t, global, "fresh", "yes")
	if reg.Get("fresh") == nil {
		t.Error("new script not registered")
	}
}

func TestSupervisorOnLoaded(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "s.lua", `ha.on_event("zha_event", function() end)`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loaded := make(chan *Runner, 1)
	sup, _, _ := newSupervisor(t, dir)
	sup.deps.OnLoaded = func(r *Runner) { loaded <- r }
	if err := sup.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); sup.Wait() }() // cancel first: Wait blocks until runners exit

	select {
	case r := <-loaded:
		types := r.EventTypes()
		if len(types) != 1 || types[0] != "zha_event" {
			t.Errorf("EventTypes = %v, want [zha_event]", types)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnLoaded never called")
	}
}
