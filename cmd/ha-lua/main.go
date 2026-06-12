package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/config"
	"github.com/sztanpet/ha-lua/internal/debug"
	"github.com/sztanpet/ha-lua/internal/ha"
	luapkg "github.com/sztanpet/ha-lua/internal/lua"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
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

	reg := luapkg.NewRegistry()
	sup := luapkg.NewSupervisor(reg, cfg.ScriptsDir, luapkg.Deps{
		Tracker: tracker,
		Global:  globalStore,
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
