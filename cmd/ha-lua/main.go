package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"

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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

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

	sup := luapkg.NewSupervisor(reg, cfg.ScriptsDir, luapkg.Deps{
		Tracker:   tracker,
		Scheduler: sched,
		Global:    globalStore,
		Router:    router,
		NewKV: func(scriptID string) *store.Store {
			return store.New(writeDB, readDB, scriptID)
		},
		CallService: func(ctx context.Context, domain, service string, data jsontext.Value) error {
			raw, err := json.Marshal(serviceCallMsg{
				ID:      client.NextID(),
				Type:    "call_service",
				Domain:  domain,
				Service: service,
				Data:    data,
			})
			if err != nil {
				return err
			}
			return client.SendRaw(ctx, raw)
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
		// AddEventType dedups and subscribes on the live connection, so
		// scripts loaded or reloaded at any time get their events.
		OnLoaded: func(r *luapkg.Runner) {
			for _, et := range r.EventTypes() {
				client.AddEventType(et)
			}
		},
	})

	// First run in a fresh add-on install: the scripts dir does not exist
	// yet, and both LoadAll and the watcher need it.
	if err := os.MkdirAll(cfg.ScriptsDir, 0o755); err != nil {
		slog.Error("scripts dir create failed", "dir", cfg.ScriptsDir, "err", err)
		os.Exit(1)
	}

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
