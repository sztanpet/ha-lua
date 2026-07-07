package ha

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
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

// serveAuth performs the server side of the auth handshake.
func serveAuth(ctx context.Context, conn *websocket.Conn) (token string, err error) {
	if err := wsjson.Write(ctx, conn, map[string]string{"type": "auth_required"}); err != nil {
		return "", err
	}
	var msg map[string]string
	if err := wsjson.Read(ctx, conn, &msg); err != nil {
		return "", err
	}
	if err := wsjson.Write(ctx, conn, map[string]string{"type": "auth_ok"}); err != nil {
		return "", err
	}
	return msg["access_token"], nil
}

// serveCommands answers get_states with the given states (echoing the
// command ID — the client's IDs increase across reconnects) and forwards
// subscribe_events types on subs. Returns when the connection drops.
func serveCommands(ctx context.Context, conn *websocket.Conn, states []map[string]any, subs chan<- string) {
	for {
		var cmd map[string]any
		if err := wsjson.Read(ctx, conn, &cmd); err != nil {
			return
		}
		switch cmd["type"] {
		case "get_states":
			_ = wsjson.Write(ctx, conn, map[string]any{
				"id": cmd["id"], "type": "result", "result": states,
			})
		case "subscribe_events":
			_ = wsjson.Write(ctx, conn, map[string]any{
				"id": cmd["id"], "type": "result", "success": true,
			})
			if subs != nil {
				et, _ := cmd["event_type"].(string)
				select {
				case subs <- et:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func testState(entityID, state string) map[string]any {
	return map[string]any{
		"entity_id": entityID, "state": state, "attributes": map[string]any{},
		"last_changed": "2026-01-01T00:00:00Z", "last_updated": "2026-01-01T00:00:00Z",
	}
}

func TestAuthFlowAndSeed(t *testing.T) {
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		token, err := serveAuth(ctx, conn)
		if err != nil {
			return
		}
		if token != "test-token" {
			t.Errorf("wrong token: %v", token)
		}
		serveCommands(ctx, conn, []map[string]any{testState("light.test", "on")}, nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "test-token")
	c.Start(ctx)

	select {
	case states := <-c.States:
		if len(states) != 1 || states[0].EntityID != "light.test" {
			t.Errorf("unexpected seed batch: %+v", states)
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

	// No seed must arrive; just verify nothing blows up and no states flow.
	select {
	case states := <-c.States:
		t.Errorf("unexpected seed batch on auth failure: %+v", states)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestEventDelivery(t *testing.T) {
	subs := make(chan string, 4)
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if _, err := serveAuth(ctx, conn); err != nil {
			return
		}
		go serveCommands(ctx, conn, nil, subs)
		select {
		case <-subs: // wait for state_changed subscription
		case <-ctx.Done():
			return
		}
		_ = wsjson.Write(ctx, conn, map[string]any{
			"type": "event",
			"id":   2,
			"event": map[string]any{
				"event_type": "state_changed",
				"time_fired": "2026-01-01T00:00:00Z",
				"data":       map[string]any{"entity_id": "light.bedroom"},
			},
		})
		<-ctx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "tok")
	c.Start(ctx)
	<-c.States

	select {
	case ev, ok := <-c.Events:
		if !ok {
			t.Fatal("events channel closed early")
		}
		if ev.Type != "state_changed" {
			t.Errorf("expected state_changed, got %q", ev.Type)
		}
		if ev.ReceivedAt.IsZero() {
			t.Error("event not stamped with ReceivedAt")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// Every reconnect must deliver a fresh seed batch — the state mirror goes
// stale across the disconnect window otherwise.
func TestReseedOnReconnect(t *testing.T) {
	var connectCount atomic.Int32
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		n := connectCount.Add(1)
		if _, err := serveAuth(ctx, conn); err != nil {
			return
		}
		state := "on"
		if n > 1 {
			state = "off"
		}
		done := make(chan struct{})
		go func() {
			serveCommands(ctx, conn, []map[string]any{testState("light.test", state)}, nil)
			close(done)
		}()
		// Drop the connection shortly after serving the first commands.
		select {
		case <-time.After(200 * time.Millisecond):
			_ = conn.Close(websocket.StatusGoingAway, "test disconnect")
		case <-ctx.Done():
		}
		<-done
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := New(wsURL(srv), "tok")
	c.Start(ctx)

	first := <-c.States
	if len(first) != 1 || first[0].State != "on" {
		t.Fatalf("unexpected first seed: %+v", first)
	}

	select {
	case second := <-c.States:
		if len(second) != 1 || second[0].State != "off" {
			t.Fatalf("unexpected re-seed batch: %+v", second)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no re-seed after reconnect")
	}
	if connectCount.Load() < 2 {
		t.Errorf("expected at least 2 connects, got %d", connectCount.Load())
	}
}

// AddEventType after the connection is up must subscribe immediately —
// waiting for the next reconnect means handlers receive nothing, possibly
// for days.
func TestAddEventTypeLiveSubscribe(t *testing.T) {
	subs := make(chan string, 8)
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if _, err := serveAuth(ctx, conn); err != nil {
			return
		}
		serveCommands(ctx, conn, nil, subs)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "tok")
	c.Start(ctx)
	<-c.States

	if et := <-subs; et != "state_changed" {
		t.Fatalf("first subscription should be state_changed, got %q", et)
	}

	c.AddEventType("zha_event")
	c.AddEventType("zha_event") // dedup: must not subscribe twice

	select {
	case et := <-subs:
		if et != "zha_event" {
			t.Errorf("expected zha_event subscription, got %q", et)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no live subscription after AddEventType")
	}

	select {
	case et := <-subs:
		t.Errorf("duplicate subscription sent: %q", et)
	case <-time.After(300 * time.Millisecond):
	}
}

// A call_service whose result HA reports as success must return nil; one HA
// rejects (success:false) must return an error carrying HA's message. This is
// the path that used to silently swallow an out-of-range set_temperature.
func TestSendCommandWaitResult(t *testing.T) {
	srv := mockServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if _, err := serveAuth(ctx, conn); err != nil {
			return
		}
		for {
			var cmd map[string]any
			if err := wsjson.Read(ctx, conn, &cmd); err != nil {
				return
			}
			switch cmd["type"] {
			case "get_states":
				_ = wsjson.Write(ctx, conn, map[string]any{"id": cmd["id"], "type": "result", "result": []any{}})
			case "subscribe_events":
				_ = wsjson.Write(ctx, conn, map[string]any{"id": cmd["id"], "type": "result", "success": true})
			case "call_service":
				// Reject a setpoint above 30, as a capped TRV would.
				data, _ := cmd["service_data"].(map[string]any)
				temp, _ := data["temperature"].(float64)
				if temp > 30 {
					_ = wsjson.Write(ctx, conn, map[string]any{"id": cmd["id"], "type": "result",
						"success": false, "error": map[string]any{"code": "invalid_format", "message": "temperature out of range"}})
				} else {
					_ = wsjson.Write(ctx, conn, map[string]any{"id": cmd["id"], "type": "result", "success": true})
				}
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := New(wsURL(srv), "tok")
	c.Start(ctx)
	<-c.States

	call := func(temp float64) error {
		id := c.NextID()
		raw := []byte(fmt.Sprintf(
			`{"id":%d,"type":"call_service","domain":"climate","service":"set_temperature","service_data":{"temperature":%v}}`,
			id, temp))
		return c.SendCommandWaitResult(ctx, id, raw)
	}

	if err := call(20); err != nil {
		t.Errorf("in-range call: unexpected error %v", err)
	}
	err := call(99)
	if err == nil {
		t.Fatal("out-of-range call: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error %q does not carry HA's message", err)
	}

	// The async form: the send returns immediately, the verdict arrives on
	// the channel — nil for success, HA's message for a rejection.
	callAsync := func(temp float64) (<-chan error, error) {
		id := c.NextID()
		raw := []byte(fmt.Sprintf(
			`{"id":%d,"type":"call_service","domain":"climate","service":"set_temperature","service_data":{"temperature":%v}}`,
			id, temp))
		return c.SendCommandAsync(ctx, id, raw)
	}

	okCh, err := callAsync(20)
	if err != nil {
		t.Fatalf("async in-range send: %v", err)
	}
	if err := <-okCh; err != nil {
		t.Errorf("async in-range verdict: unexpected error %v", err)
	}
	rejCh, err := callAsync(99)
	if err != nil {
		t.Fatalf("async out-of-range send: %v", err)
	}
	if err := <-rejCh; err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("async verdict %v does not carry HA's rejection", err)
	}
}

// The REST base is derived from the WS URL: ws→http / wss→https, the trailing
// /websocket dropped, and the result normalised to end in /api so both the
// Supervisor and a direct-HA URL resolve to the right place.
func TestDeriveRESTURL(t *testing.T) {
	cases := []struct{ ws, want string }{
		{"ws://supervisor/core/websocket", "http://supervisor/core/api"},
		{"ws://localhost:8123/api/websocket", "http://localhost:8123/api"},
		{"wss://ha.example.com/api/websocket", "https://ha.example.com/api"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := deriveRESTURL(tc.ws); got != tc.want {
			t.Errorf("deriveRESTURL(%q) = %q, want %q", tc.ws, got, tc.want)
		}
	}
}

// SetState must POST to /states/{id} with the bearer token and a JSON body,
// returning created=true only on HTTP 201. (restURL is overridden to the test
// server; New normally derives it from the WS URL.)
func TestSetState(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Path == "/states/sensor.new" {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c := New("ws://x/api/websocket", "tok")
	c.restURL = srv.URL
	ctx := context.Background()

	created, err := c.SetState(ctx, "sensor.existing", "21.5", jsontext.Value(`{"unit_of_measurement":"°C"}`))
	if err != nil {
		t.Fatalf("set existing: %v", err)
	}
	if created {
		t.Error("updating an existing entity must report created=false")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/states/sensor.existing" {
		t.Errorf("path: got %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth: got %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"state":"21.5"`) || !strings.Contains(gotBody, "°C") {
		t.Errorf("body: got %q", gotBody)
	}

	created, err = c.SetState(ctx, "sensor.new", "on", nil)
	if err != nil {
		t.Fatalf("set new: %v", err)
	}
	if !created {
		t.Error("a freshly created entity (201) must report created=true")
	}
}

// SetState surfaces an unexpected status as an error rather than silently
// succeeding.
func TestSetStateError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := New("ws://x/api/websocket", "tok")
	c.restURL = srv.URL
	if _, err := c.SetState(context.Background(), "sensor.x", "1", nil); err == nil {
		t.Fatal("expected an error on a 401 response")
	}
}

// RemoveState must DELETE /states/{id}; a 404 (already gone) is success, other
// non-200 statuses are errors.
func TestRemoveState(t *testing.T) {
	var gotMethod, gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	c := New("ws://x/api/websocket", "tok")
	c.restURL = srv.URL
	ctx := context.Background()

	if err := c.RemoveState(ctx, "sensor.x"); err != nil {
		t.Fatalf("remove (200): %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/states/sensor.x" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}

	status = http.StatusNotFound
	if err := c.RemoveState(ctx, "sensor.gone"); err != nil {
		t.Errorf("remove of an absent entity (404) must succeed, got %v", err)
	}

	status = http.StatusInternalServerError
	if err := c.RemoveState(ctx, "sensor.x"); err == nil {
		t.Error("expected an error on a 500 response")
	}
}

// With no REST base (an empty WS URL), both calls error instead of hitting a
// bogus endpoint.
func TestRESTNotConfigured(t *testing.T) {
	c := New("", "tok")
	if _, err := c.SetState(context.Background(), "sensor.x", "1", nil); err == nil {
		t.Error("set_state must error when no REST URL is configured")
	}
	if err := c.RemoveState(context.Background(), "sensor.x"); err == nil {
		t.Error("remove_state must error when no REST URL is configured")
	}
}

// A cancelled context aborts the request rather than blocking on a stuck server.
func TestSetStateContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := New("ws://x/api/websocket", "tok")
	c.restURL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SetState(ctx, "sensor.x", "1", nil); err == nil {
		t.Error("expected a context-cancelled error")
	}
}

func BenchmarkEventParsing(b *testing.B) {
	raw := []byte(`{"id":2,"type":"event","event":{"event_type":"state_changed","time_fired":"2026-01-01T00:00:00Z","data":{"entity_id":"light.bedroom","new_state":{"entity_id":"light.bedroom","state":"on","attributes":{"brightness":200}}}}}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var snap incomingMsg
		if err := json.Unmarshal(raw, &snap); err != nil {
			b.Fatal(err)
		}
		if snap.Type == "event" {
			var env eventEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				b.Fatal(err)
			}
		}
	}
}
