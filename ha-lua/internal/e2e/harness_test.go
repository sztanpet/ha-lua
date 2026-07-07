package e2e

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/sztanpet/ha-lua/internal/ha"
	luapkg "github.com/sztanpet/ha-lua/internal/lua"
	"github.com/sztanpet/ha-lua/internal/scheduler"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
)

// serviceCall is one call_service command as received by the fake server.
type serviceCall struct {
	Domain  string
	Service string
	RecvAt  time.Time
}

// fakeHA is a minimal Home Assistant WebSocket server: it answers the auth
// handshake, get_states, subscribe_events, and call_service (success after
// ackDelay — a stand-in for the device round trip HA awaits before sending
// the result frame). Received service calls are timestamped onto Calls.
type fakeHA struct {
	srv      *httptest.Server
	ackDelay time.Duration
	seed     []map[string]any
	Calls    chan serviceCall

	mu   sync.Mutex // serializes writes to conn
	conn *websocket.Conn
	// ready is closed once the client's state_changed subscription is in,
	// meaning injected events will actually be routed.
	ready     chan struct{}
	readyOnce sync.Once
}

func newFakeHA(tb testing.TB, seed []map[string]any, ackDelay time.Duration) *fakeHA {
	tb.Helper()
	f := &fakeHA{
		ackDelay: ackDelay,
		seed:     seed,
		Calls:    make(chan serviceCall, 64),
		ready:    make(chan struct{}),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f.serve(r.Context(), conn)
	}))
	tb.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHA) url() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeHA) write(ctx context.Context, v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return wsjson.Write(ctx, f.conn, v)
}

func (f *fakeHA) serve(ctx context.Context, conn *websocket.Conn) {
	f.conn = conn
	if err := wsjson.Write(ctx, conn, map[string]string{"type": "auth_required"}); err != nil {
		return
	}
	var auth map[string]string
	if err := wsjson.Read(ctx, conn, &auth); err != nil {
		return
	}
	if err := wsjson.Write(ctx, conn, map[string]string{"type": "auth_ok"}); err != nil {
		return
	}
	for {
		var cmd map[string]any
		if err := wsjson.Read(ctx, conn, &cmd); err != nil {
			return
		}
		switch cmd["type"] {
		case "get_states":
			_ = f.write(ctx, map[string]any{
				"id": cmd["id"], "type": "result", "result": f.seed,
			})
		case "subscribe_events":
			_ = f.write(ctx, map[string]any{
				"id": cmd["id"], "type": "result", "success": true,
			})
			if et, _ := cmd["event_type"].(string); et == "state_changed" {
				f.readyOnce.Do(func() { close(f.ready) })
			}
		case "call_service":
			call := serviceCall{RecvAt: time.Now()}
			call.Domain, _ = cmd["domain"].(string)
			call.Service, _ = cmd["service"].(string)
			f.Calls <- call
			// Ack from a goroutine so a delayed result never blocks reading
			// the next command — async callers must be able to overtake.
			go func(id any) {
				if f.ackDelay > 0 {
					time.Sleep(f.ackDelay)
				}
				_ = f.write(ctx, map[string]any{
					"id": id, "type": "result", "success": true,
				})
			}(cmd["id"])
		}
	}
}

// injectStateChanged pushes a state_changed event frame for entity, stamped
// with the current time the way HA stamps last_changed.
func (f *fakeHA) injectStateChanged(ctx context.Context, entity, stateVal string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	return f.write(ctx, map[string]any{
		"type": "event",
		"id":   1,
		"event": map[string]any{
			"event_type": "state_changed",
			"time_fired": now,
			"data": map[string]any{
				"entity_id": entity,
				"new_state": map[string]any{
					"entity_id": entity, "state": stateVal,
					"attributes":   map[string]any{},
					"last_changed": now, "last_updated": now,
				},
			},
		},
	})
}

func seedState(entityID, stateVal string) map[string]any {
	return map[string]any{
		"entity_id": entityID, "state": stateVal, "attributes": map[string]any{},
		"last_changed": "2026-01-01T00:00:00Z", "last_updated": "2026-01-01T00:00:00Z",
	}
}

// serviceCallMsg mirrors cmd/ha-lua's wire format for call_service.
type serviceCallMsg struct {
	ID      int            `json:"id"`
	Type    string         `json:"type"`
	Domain  string         `json:"domain"`
	Service string         `json:"service"`
	Data    jsontext.Value `json:"service_data"`
}

// pipeline wires the production components exactly like cmd/ha-lua/main.go:
// real file-backed SQLite via state.OpenDB, ha.Client against the fake
// server, the router goroutine (tracker write, then dispatch), and a
// supervisor-run Lua script. The only fakes are HA itself and the network.
type pipeline struct {
	HA     *fakeHA
	Global *store.GlobalStore
}

// startPipeline brings the whole stack up with the given script loaded and
// returns once the script signalled readiness by setting global "loaded".
func startPipeline(tb testing.TB, ctx context.Context, script string, seed []map[string]any, ackDelay time.Duration) *pipeline {
	tb.Helper()

	scriptsDir := tb.TempDir()
	if err := os.WriteFile(filepath.Join(scriptsDir, "bench.lua"), []byte(script), 0o644); err != nil {
		tb.Fatal(err)
	}

	// File-backed DB on purpose: the write path's real WAL behaviour is part
	// of what these benchmarks measure. :memory: would hide it.
	writeDB, readDB, err := state.OpenDB(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { writeDB.Close(); readDB.Close() })

	tracker := state.New(writeDB, readDB)
	tracker.Start(ctx)
	global := store.NewGlobal(writeDB, readDB)
	reg := luapkg.NewRegistry()
	sched := scheduler.New(writeDB, time.UTC, reg.DispatchToTimer)

	fake := newFakeHA(tb, seed, ackDelay)
	client := ha.New(fake.url(), "bench-token")
	client.Start(ctx)

	select {
	case states := <-client.States:
		if err := tracker.Seed(ctx, states); err != nil {
			tb.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		tb.Fatal("timed out waiting for seed")
	}

	// The router loop, verbatim from main.go.
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

	root, err := os.OpenRoot(scriptsDir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { root.Close() })

	sup := luapkg.NewSupervisor(reg, scriptsDir, luapkg.Deps{
		Tracker:   tracker,
		Scheduler: sched,
		Global:    global,
		Root:      root,
		NewKV: func(id string) *store.Store {
			return store.New(writeDB, readDB, id)
		},
		CallService: func(ctx context.Context, domain, service string, data jsontext.Value) error {
			id := client.NextID()
			raw, err := json.Marshal(serviceCallMsg{
				ID: id, Type: "call_service",
				Domain: domain, Service: service, Data: data,
			})
			if err != nil {
				return err
			}
			return client.SendCommandWaitResult(ctx, id, raw)
		},
		CallServiceAsync: func(ctx context.Context, domain, service string, data jsontext.Value) (<-chan error, error) {
			id := client.NextID()
			raw, err := json.Marshal(serviceCallMsg{
				ID: id, Type: "call_service",
				Domain: domain, Service: service, Data: data,
			})
			if err != nil {
				return nil, err
			}
			return client.SendCommandAsync(ctx, id, raw)
		},
	})
	if err := sup.LoadAll(ctx); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(sup.Wait)

	waitGlobal(tb, global, "loaded", "bench")

	select {
	case <-fake.ready:
	case <-time.After(5 * time.Second):
		tb.Fatal("timed out waiting for state_changed subscription")
	}

	return &pipeline{HA: fake, Global: global}
}

// waitGlobal polls the global KV until key holds want. Script loading is
// asynchronous.
func waitGlobal(tb testing.TB, global *store.GlobalStore, key, want string) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got any
	for time.Now().Before(deadline) {
		var err error
		got, err = global.Get(context.Background(), key)
		if err != nil {
			tb.Fatal(err)
		}
		if s, ok := got.(string); ok && s == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	tb.Fatalf("global %q = %v, want %q", key, got, want)
}

// mirrorScript reacts to switch.a like the mirrored_switches example: every
// state change becomes one service call. Immediate events — this track is
// about latency, the batch window is already opt-out.
const mirrorScript = `
ha.immediate_events()
ha.on_state_change("switch.a", function(change)
  ha.call_service("switch", "turn_" .. change.new_state.state, { entity_id = "switch.b" })
end)
global.set("loaded", "bench")
`

// mirrorScriptNoWait is mirrorScript with { wait = false }: the handler does
// not park on HA's ack, so the event loop stays free during the device
// round trip.
const mirrorScriptNoWait = `
ha.immediate_events()
ha.on_state_change("switch.a", function(change)
  ha.call_service("switch", "turn_" .. change.new_state.state, { entity_id = "switch.b" }, { wait = false })
end)
global.set("loaded", "bench")
`

func benchSeed() []map[string]any {
	return []map[string]any{
		seedState("switch.a", "off"),
		seedState("switch.b", "off"),
	}
}

// nextCall waits for one service call to arrive at the fake server.
func nextCall(tb testing.TB, p *pipeline) serviceCall {
	tb.Helper()
	select {
	case call := <-p.HA.Calls:
		return call
	case <-time.After(10 * time.Second):
		tb.Fatal("timed out waiting for service call")
		return serviceCall{}
	}
}

// toggle returns "on"/"off" alternating by iteration, so every injected
// event is a real transition.
func toggle(i int) string {
	if i%2 == 0 {
		return "on"
	}
	return "off"
}
