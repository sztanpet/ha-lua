package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/cards"
	bundled "github.com/sztanpet/ha-lua/examples"
	"github.com/sztanpet/ha-lua/internal/config"
	"github.com/sztanpet/ha-lua/internal/debug"
	"github.com/sztanpet/ha-lua/internal/ha"
	luapkg "github.com/sztanpet/ha-lua/internal/lua"
	"github.com/sztanpet/ha-lua/internal/purge"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/web"
)

func main() {
	configPath := flag.String("config", "", "Path to YAML config file (dev mode)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	// Log to stderr (the Supervisor add-on log) and, when configured, also to
	// a file under the mounted config dir so the log survives restarts and is
	// readable via the File Editor. File logging is best-effort: if the dir or
	// file cannot be opened we warn and carry on with stderr only.
	logWriter := io.Writer(os.Stderr)
	var logFileErr error
	if cfg.LogDir != "" {
		if f, err := openLogFile(cfg.LogDir); err != nil {
			logFileErr = err
		} else {
			defer f.Close()
			logWriter = io.MultiWriter(os.Stderr, f)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: level})))
	if logFileErr != nil {
		slog.Warn("file logging disabled", "dir", cfg.LogDir, "err", logFileErr)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	debug.Start(ctx, cfg.Debug.PprofAddr)

	writeDB, readDB, err := state.OpenDB(cfg.Database)
	if err != nil {
		slog.Error("db open failed", "err", err)
		os.Exit(1)
	}
	defer writeDB.Close()
	defer readDB.Close()

	tracker := state.New(writeDB, readDB)
	globalStore := store.NewGlobal(writeDB, readDB)

	loc, err := scheduler.ResolveLocation(cfg.Timezone)
	if err != nil {
		slog.Error("bad timezone", "err", err)
		os.Exit(1)
	}
	// Align the process wall-clock with the configured zone so that scripts'
	// time.now() (used by e.g. the thermostat's schedule) agrees with the
	// scheduler's ha.at. Without this, a non-UTC user on a UTC container would
	// see schedules fire at the wrong wall-clock time.
	time.Local = loc
	reg := luapkg.NewRegistry()
	router := luapkg.NewRouter(reg)
	sched := scheduler.New(writeDB, loc, reg.DispatchToTimer)
	if err := sched.Start(ctx); err != nil {
		slog.Error("scheduler start failed", "err", err)
		os.Exit(1)
	}

	purgeInterval, err := cfg.PurgeInterval()
	if err != nil {
		slog.Error("bad purge_interval", "err", err)
		os.Exit(1)
	}
	purge.New(writeDB, cfg.StateHistory.RetentionDays, purgeInterval).Start(ctx)

	// Drop the bundled reference examples beside the scripts dir, refreshed to
	// this build on every boot. Read-only reference, never loaded or run; the
	// user copies what they want into the scripts dir. Done before the HA
	// connect so the reference appears regardless of connectivity. Best-effort:
	// a failure must not stop the daemon, and an empty ExamplesDir (dev) skips it.
	if cfg.ExamplesDir != "" {
		if err := os.MkdirAll(cfg.ExamplesDir, 0o755); err != nil {
			slog.Warn("examples dir create failed", "dir", cfg.ExamplesDir, "err", err)
		} else if err := bundled.Materialize(cfg.ExamplesDir); err != nil {
			slog.Warn("examples materialize failed", "dir", cfg.ExamplesDir, "err", err)
		}
	}

	// Materialize the bundled Lovelace card assets into /config/www so HA serves
	// them at /local/ha-lua/…; refreshed to this build every boot. Best-effort,
	// and skipped (empty CardsDir) in dev.
	if cfg.CardsDir != "" {
		if err := os.MkdirAll(cfg.CardsDir, 0o755); err != nil {
			slog.Warn("cards dir create failed", "dir", cfg.CardsDir, "err", err)
		} else if err := cards.Materialize(cfg.CardsDir); err != nil {
			slog.Warn("cards materialize failed", "dir", cfg.CardsDir, "err", err)
		}
	}

	client := ha.New(cfg.HomeAssistant.URL, cfg.HomeAssistant.Token)
	client.Start(ctx)

	// Block until the first seed so scripts start against a populated mirror.
	select {
	case states := <-client.States:
		if err := tracker.Seed(ctx, states); err != nil {
			slog.Warn("state seed failed", "err", err)
		}
	case <-ctx.Done():
		return
	}

	// Every reconnect delivers a fresh batch; Seed dedups history rows.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case states := <-client.States:
				if err := tracker.Seed(ctx, states); err != nil {
					slog.Warn("state re-seed failed", "err", err)
				}
			}
		}
	}()

	// First run in a fresh add-on install: the scripts dir does not exist yet,
	// and LoadAll, the watcher, and the fs sandbox all need it present.
	if err := os.MkdirAll(cfg.ScriptsDir, 0o755); err != nil {
		slog.Error("scripts dir create failed", "dir", cfg.ScriptsDir, "err", err)
		os.Exit(1)
	}

	// One process-wide os.Root backing the read-only Lua fs module. It is
	// goroutine-safe and shared across all script LStates; held for the
	// process lifetime.
	scriptsRoot, err := os.OpenRoot(cfg.ScriptsDir)
	if err != nil {
		slog.Error("scripts dir open failed", "dir", cfg.ScriptsDir, "err", err)
		os.Exit(1)
	}
	defer scriptsRoot.Close()

	sup := luapkg.NewSupervisor(reg, cfg.ScriptsDir, luapkg.Deps{
		Tracker:   tracker,
		Scheduler: sched,
		Global:    globalStore,
		Root:      scriptsRoot,
		Router:    router,
		NewKV: func(scriptID string) *store.Store {
			return store.New(writeDB, readDB, scriptID)
		},
		CallService: func(ctx context.Context, domain, service string, data jsontext.Value) error {
			// Wait for HA's result so a rejected call (e.g. a setpoint above
			// the device's max_temp) surfaces as an error to the script rather
			// than vanishing — fire-and-forget hid those failures entirely.
			id := client.NextID()
			raw, err := json.Marshal(serviceCallMsg{
				ID:      id,
				Type:    "call_service",
				Domain:  domain,
				Service: service,
				Data:    data,
			})
			if err != nil {
				return err
			}
			return client.SendCommandWaitResult(ctx, id, raw)
		},
		FireEvent: func(ctx context.Context, eventType string, data jsontext.Value) error {
			raw, err := json.Marshal(fireEventMsg{
				ID:        client.NextID(),
				Type:      "fire_event",
				EventType: eventType,
				EventData: data,
			})
			if err != nil {
				return err
			}
			return client.SendRaw(ctx, raw)
		},
		// Entity publish/remove ride the core REST API; the bindings are
		// non-raising so the per-minute publish doesn't spam on_exception
		// during a transient outage.
		SetState:    client.SetState,
		RemoveState: client.RemoveState,
		// AddEventType dedups and subscribes on the live connection, so
		// scripts loaded or reloaded at any time get their events.
		OnLoaded: func(r *luapkg.Runner) {
			for _, et := range r.EventTypes() {
				client.AddEventType(et)
			}
		},
	})

	// Watch before the initial load: a script created in between is then
	// a queued event instead of a file nobody ever looks at.
	watcher, err := luapkg.NewScriptWatcher(cfg.ScriptsDir)
	if err != nil {
		slog.Warn("script watcher failed, hot reload disabled", "err", err)
	}
	if err := sup.LoadAll(ctx); err != nil {
		slog.Error("script load failed", "err", err)
		os.Exit(1)
	}
	if watcher != nil {
		go watcher.Run(ctx, sup)
	}

	// LAN port (dashboard Webpage card + dev) and the HA ingress port
	// (authenticated sidebar panel) are two entry points onto the same router.
	if cfg.HTTPPort != 0 {
		web.Start(ctx, fmt.Sprintf(":%d", cfg.HTTPPort), router)
	}
	if cfg.IngressPort != 0 && cfg.IngressPort != cfg.HTTPPort {
		web.Start(ctx, fmt.Sprintf(":%d", cfg.IngressPort), router)
	}

	// Route HA events to state tracker and all runners.
	go func() {
		for ev := range client.Events {
			if ev.Type == "state_changed" {
				if err := tracker.HandleStateChanged(ctx, ev.Data); err != nil {
					slog.Warn("state tracker error", "err", err)
				}
			}
			reg.Dispatch(ev)
		}
	}()

	<-ctx.Done()
	sup.Wait()
}

// openLogFile creates dir if needed and opens (appending) the daemon's log
// file inside it. The caller keeps the handle for the process lifetime and
// closes it on exit; the return type is io.WriteCloser because that deferred
// close is the only thing the caller does with it besides writing.
func openLogFile(dir string) (io.WriteCloser, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "ha-lua.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

type serviceCallMsg struct {
	ID      int            `json:"id"`
	Type    string         `json:"type"`
	Domain  string         `json:"domain"`
	Service string         `json:"service"`
	Data    jsontext.Value `json:"service_data"`
}

type fireEventMsg struct {
	ID        int            `json:"id"`
	Type      string         `json:"type"`
	EventType string         `json:"event_type"`
	EventData jsontext.Value `json:"event_data"`
}
