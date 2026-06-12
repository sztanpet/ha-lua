package lua

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceDelay is how long a script file must stay quiet before it is
// acted on. Editors fire bursts of events per save, and atomic saves
// (write temp file + rename over the target) look like several distinct
// operations.
const debounceDelay = 300 * time.Millisecond

// ScriptWatcher watches a script directory for *.lua changes.
type ScriptWatcher struct {
	dir string
	w   *fsnotify.Watcher
}

// NewScriptWatcher starts watching dir. The watch is active as soon as
// this returns — events queue up until Run drains them — so there is no
// window between an initial LoadAll and watching where a change could be
// missed.
func NewScriptWatcher(dir string) (*ScriptWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, err
	}
	return &ScriptWatcher{dir: dir, w: w}, nil
}

// Run routes file changes through sup: created or modified scripts are
// (re)loaded, deleted scripts are stopped. Blocks until ctx is done.
func (sw *ScriptWatcher) Run(ctx context.Context, sup *Supervisor) {
	defer sw.w.Close()
	slog.Info("lua: watching scripts for changes", "dir", sw.dir)

	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, t := range timers {
			t.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-sw.w.Errors:
			if !ok {
				return
			}
			slog.Warn("lua: watcher error", "err", err)
		case ev, ok := <-sw.w.Events:
			if !ok {
				return
			}
			if !ev.Op.Has(fsnotify.Create) && !ev.Op.Has(fsnotify.Write) &&
				!ev.Op.Has(fsnotify.Remove) && !ev.Op.Has(fsnotify.Rename) {
				continue
			}
			base := filepath.Base(ev.Name)
			if !strings.HasSuffix(base, ".lua") || strings.HasPrefix(base, ".") {
				continue
			}
			id := strings.TrimSuffix(base, ".lua")
			path := filepath.Join(sw.dir, base)

			// Debounce per script, then act on the settled state of the
			// file: present means (re)load, absent means stop. Deciding
			// from the event type would get atomic saves wrong.
			mu.Lock()
			if t, ok := timers[id]; ok {
				t.Reset(debounceDelay)
				mu.Unlock()
				continue
			}
			timers[id] = time.AfterFunc(debounceDelay, func() {
				mu.Lock()
				delete(timers, id)
				mu.Unlock()
				if ctx.Err() != nil {
					return
				}
				if _, err := os.Stat(path); err == nil {
					slog.Info("lua: reloading script", "script", id)
					sup.Reload(ctx, id)
				} else {
					slog.Info("lua: script removed, stopping", "script", id)
					sup.StopScript(id)
				}
			})
			mu.Unlock()
		}
	}
}
