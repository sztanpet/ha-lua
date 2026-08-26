package lua

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sztanpet/ha-lua/internal/mqtt"
	"github.com/sztanpet/ha-lua/internal/state"
	"github.com/sztanpet/ha-lua/internal/store"
	"github.com/sztanpet/ha-lua/internal/testutil"
)

type fakeBroker struct {
	mu        sync.Mutex
	filters   []string
	published []string // "topic payload"
	err       error
}

func (b *fakeBroker) subscribe(filter string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.filters = append(b.filters, filter)
	return nil
}

func (b *fakeBroker) publish(topic string, payload []byte, _ byte, _ bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.published = append(b.published, topic+" "+string(payload))
	return nil
}

func (b *fakeBroker) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.published...)
}

// newMQTTRunner starts one script with a fake broker wired in.
func newMQTTRunner(t *testing.T, script string, wire bool) (*Runner, *fakeBroker, *Registry) {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, dir, "m.lua", script)

	writeDB, readDB := testutil.NewTestDB(t, func(writeDB, _ *sql.DB) error {
		return state.Migrate(writeDB)
	})
	global := store.NewGlobal(writeDB, readDB)
	reg := NewRegistry()
	broker := &fakeBroker{}

	r := NewRunner("m", dir, openTestRoot(t, dir), nil, nil, nil,
		store.New(writeDB, readDB, "m"), global)
	if wire {
		r.SetMQTT(broker.subscribe, broker.publish)
	}
	reg.Add(r)

	ctx := t.Context()
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx, filepath.Join(dir, "m.lua")) }()
	t.Cleanup(func() { <-done })
	select {
	case <-r.LoadedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("script did not load")
	}
	return r, broker, reg
}

// TestMQTTSubscribeAndDeliver is the round trip through the registry: only the
// scripts whose filter matches see the message, and a JSON payload arrives
// decoded alongside the raw string.
func TestMQTTSubscribeAndDeliver(t *testing.T) {
	_, broker, reg := newMQTTRunner(t, `
seen = {}
mqtt.subscribe("zigbee2mqtt/ikea dimmer 1/action", function(topic, payload, decoded)
  store.set("last", topic .. "|" .. payload .. "|" .. tostring(decoded))
end)
mqtt.subscribe("zigbee2mqtt/+/state", function(topic, payload, decoded)
  store.set("json", tostring(decoded and decoded.brightness))
end)
`, true)

	if got := len(broker.filters); got != 2 {
		t.Fatalf("broker filters = %v, want the two the script asked for", broker.filters)
	}

	reg.DispatchMQTT(mqtt.Message{Topic: "zigbee2mqtt/ikea dimmer 1/action",
		Payload: []byte("brightness_move_down")})
	reg.DispatchMQTT(mqtt.Message{Topic: "zigbee2mqtt/lamp/state",
		Payload: []byte(`{"brightness":120}`)})
	// Not subscribed: must never reach the script.
	reg.DispatchMQTT(mqtt.Message{Topic: "zigbee2mqtt/other/thing", Payload: []byte("x")})

	kv := reg.Get("m").kv
	waitFor(t, "the action handler to run", func() bool { v, _ := kv.Get(t.Context(), "last"); return v != nil })
	last, _ := kv.Get(t.Context(), "last")
	if last != "zigbee2mqtt/ikea dimmer 1/action|brightness_move_down|nil" {
		t.Errorf("handler args = %v", last)
	}
	waitFor(t, "the JSON handler to run", func() bool { v, _ := kv.Get(t.Context(), "json"); return v != nil })
	if v, _ := kv.Get(t.Context(), "json"); v != "120" {
		t.Errorf("decoded JSON payload = %v, want 120", v)
	}
}

// TestMQTTPublishEncoding: a string goes out verbatim (Zigbee2MQTT's action
// topics are bare strings), a table goes out as JSON.
func TestMQTTPublishEncoding(t *testing.T) {
	_, broker, _ := newMQTTRunner(t, `
mqtt.publish("zigbee2mqtt/light1/set", { brightness_move = -40 })
mqtt.publish("zigbee2mqtt/light1/set/state", "ON")
`, true)

	sent := broker.sent()
	if len(sent) != 2 {
		t.Fatalf("published = %v, want 2", sent)
	}
	if sent[0] != `zigbee2mqtt/light1/set {"brightness_move":-40}` {
		t.Errorf("table payload = %q", sent[0])
	}
	if sent[1] != "zigbee2mqtt/light1/set/state ON" {
		t.Errorf("string payload = %q", sent[1])
	}
}

// TestMQTTWithoutBrokerRaises: no broker configured must fail the script
// loudly. Silently doing nothing is the failure mode this subsystem exists to
// end.
func TestMQTTWithoutBrokerRaises(t *testing.T) {
	r, _, _ := newMQTTRunner(t, `mqtt.subscribe("a/#", function() end)`, false)
	if !scriptRaised(r) {
		t.Error("mqtt.subscribe with no broker did not raise")
	}
}

// TestMQTTBadFilterRaisesAtLoad keeps a typo from becoming a subscription
// that never fires.
func TestMQTTBadFilterRaisesAtLoad(t *testing.T) {
	r, broker, _ := newMQTTRunner(t, `mqtt.subscribe("sport/#/ranking", function() end)`, true)
	if !scriptRaised(r) {
		t.Error("a '#' in the middle of a filter did not raise")
	}
	if len(broker.filters) != 0 {
		t.Errorf("broker saw %v, want nothing for an invalid filter", broker.filters)
	}
}

// scriptRaised reports whether the script recorded an error.
func scriptRaised(r *Runner) bool { return r.Stats().LastError != nil }
