package lua

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitFor polls cond until it returns true or the deadline expires.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatchScripts(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup, reg, global := newSupervisor(t, dir)
	sw, err := NewScriptWatcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		sw.Run(ctx, sup)
	}()
	defer func() {
		cancel()
		<-watcherDone
		sup.Wait()
	}()

	// Created file gets loaded.
	writeScript(t, dir, "s.lua", `global.set("ver", "v1")`)
	globalString(t, global, "ver", "v1")
	waitFor(t, "runner registered", func() bool { return reg.Get("s") != nil })

	// Modified file gets reloaded with the new content.
	first := reg.Get("s")
	writeScript(t, dir, "s.lua", `global.set("ver", "v2")`)
	globalString(t, global, "ver", "v2")
	waitFor(t, "runner replaced", func() bool {
		r := reg.Get("s")
		return r != nil && r != first
	})

	// Atomic save (write temp + rename over target), the way editors do it.
	tmp := filepath.Join(dir, ".s.lua.tmp")
	if err := os.WriteFile(tmp, []byte(`global.set("ver", "v3")`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "s.lua")); err != nil {
		t.Fatal(err)
	}
	globalString(t, global, "ver", "v3")

	// Removed file gets stopped.
	if err := os.Remove(filepath.Join(dir, "s.lua")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runner removed", func() bool { return reg.Get("s") == nil })

	// Non-lua files are ignored.
	writeScript(t, dir, "notes.txt", "nothing")
	time.Sleep(2 * debounceDelay)
	if reg.Get("notes") != nil {
		t.Error("non-lua file got a runner")
	}
}
