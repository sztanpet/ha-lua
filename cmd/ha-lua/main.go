package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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

	reg := luapkg.NewRegistry()
	scriptPaths := make(map[string]string)

	entries, _ := os.ReadDir(cfg.ScriptsDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		scriptID := strings.TrimSuffix(e.Name(), ".lua")
		path := filepath.Join(cfg.ScriptsDir, e.Name())
		kv := store.New(writeDB, readDB, scriptID)
		runner := luapkg.NewRunner(scriptID, cfg.ScriptsDir, tracker, kv, globalStore)
		reg.Add(runner)
		scriptPaths[scriptID] = path
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

	// Wire call_service and fire_event through the HA client.
	makeCallService := func() func(ctx context.Context, domain, service string, data jsontext.Value) error {
		return func(ctx context.Context, domain, service string, data jsontext.Value) error {
			msg := serviceCallMsg{
				ID:      client.NextID(),
				Type:    "call_service",
				Domain:  domain,
				Service: service,
				Data:    data,
			}
			raw, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			return client.SendRaw(ctx, raw)
		}
	}

	makeFireEvent := func() func(ctx context.Context, eventType string, data jsontext.Value) error {
		return func(ctx context.Context, eventType string, data jsontext.Value) error {
			msg := fireEventMsg{
				ID:        client.NextID(),
				Type:      "fire_event",
				EventType: eventType,
				EventData: data,
			}
			raw, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			return client.SendRaw(ctx, raw)
		}
	}

	for _, r := range reg.All() {
		r.SetCallService(makeCallService())
		r.SetFireEvent(makeFireEvent())
	}

	var wg sync.WaitGroup
	for _, r := range reg.All() {
		path := scriptPaths[r.ScriptID()]
		if path == "" {
			continue
		}
		wg.Add(1)
		go func(runner *luapkg.Runner, p string) {
			defer wg.Done()
			runner.Start(ctx, p)
		}(r, path)
	}

	// After all runners are loaded, subscribe to any custom event types.
	go func() {
		for _, r := range reg.All() {
			select {
			case <-r.LoadedCh:
			case <-ctx.Done():
				return
			}
		}
		for _, et := range reg.EventTypes() {
			client.AddEventType(et)
		}
	}()

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

	wg.Wait()
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
