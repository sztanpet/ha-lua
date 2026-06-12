package ha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// mockServer runs a minimal HA WebSocket server for testing.
func mockServer(t *testing.T, handler func(ctx context.Context, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("accept error: %v", err)
			return
		}
		defer conn.CloseNow()
		handler(r.Context(), conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestAuthFlow(t *testing.T) {
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		// Send auth_required
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_required"})
		// Read auth
		var msg map[string]string
		_ = wsjson.Read(ctx, conn, &msg)
		if msg["type"] != "auth" {
			t.Errorf("expected auth msg, got %v", msg)
		}
		if msg["access_token"] != "test-token" {
			t.Errorf("wrong token: %v", msg["access_token"])
		}
		// Send auth_ok
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_ok"})
		// Send get_states result
		_ = wsjson.Write(ctx, conn, map[string]any{
			"id":     1,
			"type":   "result",
			"result": []map[string]any{{"entity_id": "light.test", "state": "on", "attributes": "{}", "last_changed": "2026-01-01T00:00:00Z", "last_updated": "2026-01-01T00:00:00Z"}},
		})
		// Keep alive briefly
		<-ctx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "test-token")
	c.Start(ctx)

	// Should receive seeded states
	select {
	case states := <-c.States:
		if len(states) == 0 {
			t.Error("expected at least one state from seed")
		}
		if states[0].EntityID != "light.test" {
			t.Errorf("unexpected entity_id: %v", states[0].EntityID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for states")
	}
}

func TestAuthInvalid(t *testing.T) {
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_required"})
		var msg map[string]string
		_ = wsjson.Read(ctx, conn, &msg)
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_invalid"})
		<-ctx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "bad-token")
	c.Start(ctx)

	// States channel should close quickly (on error, seedDone never set)
	// Just verify we don't hang
	time.Sleep(200 * time.Millisecond)
	cancel()
}

func TestEventDelivery(t *testing.T) {
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_required"})
		var msg map[string]string
		_ = wsjson.Read(ctx, conn, &msg)
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_ok"})
		// get_states result
		_ = wsjson.Write(ctx, conn, map[string]any{
			"id":     1,
			"type":   "result",
			"result": []any{},
		})
		// Wait for subscribe message(s)
		for {
			var sub map[string]any
			if err := wsjson.Read(ctx, conn, &sub); err != nil {
				return
			}
			if sub["type"] == "subscribe_events" {
				break
			}
		}
		// Send a state_changed event
		_ = wsjson.Write(ctx, conn, map[string]any{
			"type": "event",
			"id":   2,
			"event": map[string]any{
				"event_type": "state_changed",
				"time_fired": "2026-01-01T00:00:00Z",
				"data":       json.RawMessage(`{"entity_id":"light.bedroom"}`),
			},
		})
		<-ctx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "tok")
	c.Start(ctx)

	// Drain states
	<-c.States

	select {
	case ev, ok := <-c.Events:
		if !ok {
			t.Fatal("events channel closed early")
		}
		if ev.Type != "state_changed" {
			t.Errorf("expected state_changed, got %q", ev.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestReconnect(t *testing.T) {
	var connectCount atomic.Int32
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		connectCount.Add(1)
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_required"})
		var msg map[string]string
		_ = wsjson.Read(ctx, conn, &msg)
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "auth_ok"})
		_ = wsjson.Write(ctx, conn, map[string]any{"id": 1, "type": "result", "result": []any{}})
		_ = conn.Close(websocket.StatusGoingAway, "test disconnect")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "tok")
	c.Start(ctx)
	<-c.States

	// Wait for at least one reconnect
	time.Sleep(1500 * time.Millisecond)
	if n := connectCount.Load(); n < 2 {
		t.Errorf("expected reconnect, got %d connects", n)
	}
}
