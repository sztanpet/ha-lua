package mqtt

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// newBroker runs a real MQTT broker in-process on a free port. A protocol
// client is worth testing against the protocol, not against a fake that
// agrees with whatever the client happens to send.
func newBroker(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	server := mochi.New(&mochi.Options{InlineClient: true})
	if err := server.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	if err := server.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: addr})); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	return "tcp://" + addr
}

type recorder struct {
	mu   sync.Mutex
	msgs []Message
	ch   chan Message
}

func newRecorder() *recorder { return &recorder{ch: make(chan Message, 16)} }

func (r *recorder) handle(m Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	select {
	case r.ch <- m:
	default:
	}
}

func (r *recorder) expect(t *testing.T, topic, payload string) {
	t.Helper()
	select {
	case m := <-r.ch:
		if m.Topic != topic || string(m.Payload) != payload {
			t.Fatalf("message = %s %q, want %s %q", m.Topic, m.Payload, topic, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no message, want %s %q", topic, payload)
	}
}

func (r *recorder) expectSilence(t *testing.T) {
	t.Helper()
	select {
	case m := <-r.ch:
		t.Fatalf("unexpected message %s %q", m.Topic, m.Payload)
	case <-time.After(300 * time.Millisecond):
	}
}

func startClient(t *testing.T, broker string, rec *recorder) *Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := New(Config{Broker: broker, ClientID: fmt.Sprintf("test-%d", time.Now().UnixNano())}, rec.handle)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRoundTrip is the whole contract in one: subscribe, publish, receive,
// against a real broker.
func TestRoundTrip(t *testing.T) {
	broker := newBroker(t)
	rec := newRecorder()
	c := startClient(t, broker, rec)

	if err := c.Subscribe("zigbee2mqtt/+/action"); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish("zigbee2mqtt/dimmer/action", []byte("brightness_move_down"), 0, false); err != nil {
		t.Fatal(err)
	}
	rec.expect(t, "zigbee2mqtt/dimmer/action", "brightness_move_down")

	// A topic the filter does not cover must not be delivered.
	if err := c.Publish("zigbee2mqtt/dimmer/state", []byte("on"), 0, false); err != nil {
		t.Fatal(err)
	}
	rec.expectSilence(t)
}

// TestSubscribeDedups: many scripts asking for the same filter must not turn
// into many broker subscriptions — and must not turn one message into many.
func TestSubscribeDedups(t *testing.T) {
	broker := newBroker(t)
	rec := newRecorder()
	c := startClient(t, broker, rec)

	for range 3 {
		if err := c.Subscribe("home/#"); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Stats().Filters; len(got) != 1 || got[0] != "home/#" {
		t.Fatalf("filters = %v, want [home/#]", got)
	}
	if err := c.Publish("home/kitchen/light", []byte("on"), 0, false); err != nil {
		t.Fatal(err)
	}
	rec.expect(t, "home/kitchen/light", "on")
	rec.expectSilence(t) // exactly one delivery, not three
}

// TestSubscribeBeforeStartIsLive is the regression test for the race that
// CleanSession creates: filters registered before Start are sent by the
// OnConnect handler, which used to run after Start returned — so a publish
// issued straight after Start beat its own SUBSCRIBE to the broker and the
// message was dropped. Start must not return until that pass is done.
func TestSubscribeBeforeStartIsLive(t *testing.T) {
	broker := newBroker(t)
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := New(Config{Broker: broker, ClientID: "test-early-live"}, rec.handle)
	if err := c.Subscribe("home/#"); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish("home/kitchen/light", []byte("on"), 0, false); err != nil {
		t.Fatal(err)
	}
	rec.expect(t, "home/kitchen/light", "on")
}

// TestSubscribeBeforeConnectSurvives: a filter registered while the broker is
// unreachable must be sent once the connection comes up, since OnConnect is
// the only thing that restores subscriptions after CleanSession drops them.
func TestSubscribeBeforeConnectSurvives(t *testing.T) {
	rec := newRecorder()
	c := New(Config{Broker: "tcp://127.0.0.1:1", ClientID: "test-early"}, rec.handle)
	if err := c.Subscribe("early/#"); err != nil {
		t.Fatalf("Subscribe before Start: %v", err)
	}
	if got := c.Stats().Filters; len(got) != 1 {
		t.Fatalf("filters = %v, want the filter recorded before connect", got)
	}
	if c.Stats().Connected {
		t.Error("Connected = true with no broker running")
	}
}

// TestDisabledClient: with no broker configured every operation must fail
// loudly rather than silently doing nothing.
func TestDisabledClient(t *testing.T) {
	c := New(Config{}, func(Message) {})
	if c.Enabled() {
		t.Error("Enabled = true with no broker")
	}
	if err := c.Start(t.Context()); err != ErrDisabled {
		t.Errorf("Start = %v, want ErrDisabled", err)
	}
	if err := c.Subscribe("a/#"); err != ErrDisabled {
		t.Errorf("Subscribe = %v, want ErrDisabled", err)
	}
	if err := c.Publish("a", []byte("b"), 0, false); err != ErrDisabled {
		t.Errorf("Publish = %v, want ErrDisabled", err)
	}
}

// TestBadCredentialsReportTheReason is the diagnostics regression test: with
// paho's own connect-retry enabled a rejected CONNECT leaves the token
// pending, so the daemon reported "connect timed out" while the broker had
// actually said "not Authorized". That cost a real debugging session against
// a live broker; the reason must reach the caller.
func TestBadCredentialsReportTheReason(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	server := mochi.New(&mochi.Options{InlineClient: true})
	err = server.AddHook(new(auth.Hook), &auth.Options{
		Ledger: &auth.Ledger{Auth: auth.AuthRules{
			{Username: "right", Password: "right", Allow: true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.AddListener(listeners.NewTCP(listeners.Config{ID: "auth", Address: addr})); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })

	c := New(Config{Broker: "tcp://" + addr, Username: "wrong", Password: "wrong",
		ClientID: "test-badcreds"}, func(Message) {})
	err = c.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded with rejected credentials")
	}
	if !strings.Contains(err.Error(), "Authorized") {
		t.Errorf("Start error = %v, want the broker's own verdict, not a timeout", err)
	}
}

// TestSubscribeRejectsBadFilter keeps the load-time validation on the path
// scripts actually call.
func TestSubscribeRejectsBadFilter(t *testing.T) {
	broker := newBroker(t)
	c := startClient(t, broker, newRecorder())
	if err := c.Subscribe("sport/#/ranking"); err == nil {
		t.Error("Subscribe accepted a '#' in the middle of a filter")
	}
}
