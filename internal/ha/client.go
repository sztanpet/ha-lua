package ha

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Client connects to the HA WebSocket API. It handles auth, seeding, and
// subscription management. Consumers receive events on the Events channel.
type Client struct {
	url    string
	token  string
	msgID  atomic.Int32

	// Events is closed when the client shuts down.
	Events chan Event

	// States delivers the initial seed batch from get_states; closed after seed.
	States chan []StateData

	// subscriptions requested before/after connect; protected by Reconnect logic.
	extraEventTypes []string

	// conn holds the current WebSocket connection. coder/websocket serializes
	// concurrent writes internally, so callers may write from any goroutine.
	conn atomic.Pointer[websocket.Conn]
}

// New creates a Client. Call Start to begin connecting.
func New(url, token string) *Client {
	return &Client{
		url:    url,
		token:  token,
		Events: make(chan Event, 256),
		States: make(chan []StateData, 1),
	}
}

// AddEventType registers an event type that should be subscribed to on
// every connect/reconnect. Must be called before Start.
func (c *Client) AddEventType(t string) {
	c.extraEventTypes = append(c.extraEventTypes, t)
}

// NextID returns the next outbound message ID.
func (c *Client) NextID() int {
	return c.nextID()
}

// SendRaw writes raw JSON bytes as a WebSocket text message. Returns an error
// if no connection is active. coder/websocket serializes concurrent writes.
func (c *Client) SendRaw(ctx context.Context, data []byte) error {
	conn := c.conn.Load()
	if conn == nil {
		return fmt.Errorf("ha: not connected")
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// Start runs the connection loop in a background goroutine. Blocks until
// ctx is cancelled.
func (c *Client) Start(ctx context.Context) {
	go c.loop(ctx)
}

func (c *Client) loop(ctx context.Context) {
	defer close(c.Events)
	backoff := time.Second
	seedDone := false
	for {
		if err := c.connect(ctx, &seedDone); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("ha: connection failed, retrying", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (c *Client) connect(ctx context.Context, seedDone *bool) error {
	conn, _, err := websocket.Dial(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	c.conn.Store(conn)
	defer c.conn.Store(nil)

	if err := c.auth(ctx, conn); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Reset msgID on each connection
	c.msgID.Store(0)

	// Seed states on first connect only
	if !*seedDone {
		states, err := c.getStates(ctx, conn)
		if err != nil {
			return fmt.Errorf("get_states: %w", err)
		}
		select {
		case c.States <- states:
		default:
		}
		close(c.States)
		*seedDone = true
	}

	// Subscribe to state_changed always, plus any registered extra types
	eventTypes := append([]string{"state_changed"}, c.extraEventTypes...)
	for _, et := range eventTypes {
		if err := c.subscribe(ctx, conn, et); err != nil {
			return fmt.Errorf("subscribe %q: %w", et, err)
		}
	}

	// Read loop
	return c.readLoop(ctx, conn)
}

func (c *Client) nextID() int {
	return int(c.msgID.Add(1))
}

func (c *Client) auth(ctx context.Context, conn *websocket.Conn) error {
	var msg incomingMsg
	if err := wsjson.Read(ctx, conn, &msg); err != nil {
		return err
	}
	if msg.Type != "auth_required" {
		return fmt.Errorf("expected auth_required, got %q", msg.Type)
	}
	if err := wsjson.Write(ctx, conn, authMsg{Type: "auth", AccessToken: c.token}); err != nil {
		return err
	}
	if err := wsjson.Read(ctx, conn, &msg); err != nil {
		return err
	}
	switch msg.Type {
	case "auth_ok":
		slog.Info("ha: authenticated")
		return nil
	case "auth_invalid":
		return fmt.Errorf("auth_invalid: check token")
	default:
		return fmt.Errorf("unexpected auth response %q", msg.Type)
	}
}

func (c *Client) getStates(ctx context.Context, conn *websocket.Conn) ([]StateData, error) {
	id := c.nextID()
	if err := wsjson.Write(ctx, conn, commandMsg{ID: id, Type: "get_states"}); err != nil {
		return nil, err
	}
	// Read until we get the result for our command ID
	for {
		raw, err := readRaw(ctx, conn)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			ID   int    `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			continue
		}
		if envelope.Type == "result" && envelope.ID == id {
			var result getStatesResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
			return result.Result, nil
		}
	}
}

func (c *Client) subscribe(ctx context.Context, conn *websocket.Conn, eventType string) error {
	id := c.nextID()
	return wsjson.Write(ctx, conn, subscribeMsg{
		ID:        id,
		Type:      "subscribe_events",
		EventType: eventType,
	})
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		raw, err := readRaw(ctx, conn)
		if err != nil {
			return err
		}
		var snap incomingMsg
		if err := json.Unmarshal(raw, &snap); err != nil {
			slog.Warn("ha: failed to parse message", "err", err)
			continue
		}
		if snap.Type != "event" {
			continue
		}
		var env eventEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			slog.Warn("ha: failed to parse event", "err", err)
			continue
		}
		select {
		case c.Events <- env.Event:
		case <-ctx.Done():
			return ctx.Err()
		default:
			slog.Warn("ha: event channel full, dropping event", "type", env.Event.Type)
		}
	}
}

func readRaw(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	_, data, err := conn.Read(ctx)
	return data, err
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
