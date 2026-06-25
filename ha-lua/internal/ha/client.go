// Package ha implements the Home Assistant WebSocket client: auth,
// reconnect with backoff, state seeding, and the raw event stream.
package ha

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-json-experiment/json"
)

// readLimit caps the size of a single inbound WebSocket message. get_states
// returns every entity's full state in one frame, which on a large install
// runs to several megabytes — far past coder/websocket's 32 KiB default.
const readLimit = 64 << 20 // 64 MiB

// Client connects to the HA WebSocket API. It handles auth, seeding, and
// subscription management. Consumers receive events on the Events channel.
// The same instance also drives the core REST API (SetState/RemoveState),
// which shares the WS token.
type Client struct {
	url   string
	token string

	// restURL is the base of the core REST API (…/api), derived from the WS
	// URL in New and used by SetState and RemoveState. Empty when no WS URL was
	// given, in which case those calls return an error rather than hitting a
	// bogus endpoint.
	restURL    string
	httpClient *http.Client

	// msgID increases monotonically across reconnects. HA only requires
	// IDs to increase within a connection, so never resetting it is both
	// valid and immune to races with concurrent SendRaw callers.
	msgID atomic.Int32

	// Events is closed when the client shuts down.
	Events chan Event

	// States delivers one batch of get_states results per (re)connect —
	// the plan requires re-seeding on every reconnect, the mirror goes
	// stale across the disconnect window otherwise. Capacity 1, newest
	// batch wins, never closed.
	States chan []StateData

	mu         sync.Mutex
	conn       *websocket.Conn     // authed connection; nil while down
	eventTypes []string            // extra types beyond state_changed
	subscribed map[string]struct{} // types subscribed on the current conn

	// pending correlates a command id with the goroutine awaiting HA's
	// result frame. Guarded by mu and drained on disconnect, so a waiter
	// never blocks past the life of the connection it was sent on.
	pending map[int]chan cmdResult
}

// cmdResult carries HA's outcome for a correlated command back to the waiting
// caller of SendCommandWaitResult.
type cmdResult struct {
	success bool
	errMsg  string
}

// New creates a Client for the WebSocket endpoint url. The core REST API base
// (used by SetState/RemoveState) is derived from url — the WS and REST
// endpoints share a host and token but differ in scheme and path tail. token
// authenticates both. Call Start to begin connecting.
func New(url, token string) *Client {
	return &Client{
		url:        url,
		token:      token,
		restURL:    deriveRESTURL(url),
		httpClient: &http.Client{Timeout: restTimeout},
		Events:     make(chan Event, 256),
		States:     make(chan []StateData, 1),
		pending:    make(map[int]chan cmdResult),
	}
}

// AddEventType registers an event type to subscribe to, in addition to
// state_changed. Safe to call at any time from any goroutine: if a
// connection is up the subscription is sent immediately, and every
// reconnect re-subscribes the full set.
func (c *Client) AddEventType(t string) {
	c.mu.Lock()
	if slices.Contains(c.eventTypes, t) {
		c.mu.Unlock()
		return
	}
	c.eventTypes = append(c.eventTypes, t)
	conn := c.conn
	send := false
	if conn != nil {
		if _, ok := c.subscribed[t]; !ok {
			// Mark before sending: if the send fails the connection is
			// dead and the reconnect resets the subscribed set anyway.
			c.subscribed[t] = struct{}{}
			send = true
		}
	}
	c.mu.Unlock()

	if !send {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.subscribe(ctx, conn, t); err != nil {
		slog.Warn("ha: live subscribe failed, retrying on reconnect", "type", t, "err", err)
	}
}

// NextID returns the next outbound message ID.
func (c *Client) NextID() int {
	return c.nextID()
}

// SendRaw writes raw JSON bytes as a WebSocket text message. Returns an error
// if no authenticated connection is active. coder/websocket serializes
// concurrent writes.
func (c *Client) SendRaw(ctx context.Context, data []byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("ha: not connected")
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// SendCommandWaitResult writes a command and blocks until HA returns the result
// frame for its id, returning an error if HA reports the command failed (for
// example a set_temperature above the device's max_temp), if no connection is
// active, or if ctx is cancelled. The id must be the one embedded in data
// (allocate it with NextID). Unlike SendRaw, this surfaces HA-side rejections,
// which the WebSocket protocol reports asynchronously in a separate frame.
func (c *Client) SendCommandWaitResult(ctx context.Context, id int, data []byte) error {
	// Bound the wait so a command HA never answers can't pin the calling
	// script goroutine forever. HA answers every command in practice; this is
	// only a liveness floor on top of the disconnect drain.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ch := make(chan cmdResult, 1)
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return fmt.Errorf("ha: not connected")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	select {
	case res := <-ch:
		if !res.success {
			return fmt.Errorf("ha: command rejected: %s", res.errMsg)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// deliverResult routes a "result" frame to the goroutine waiting on its id.
// Frames with no waiter (subscribe acks, the inline get_states result) are
// dropped. The waiter's channel is buffered (cap 1) with a single reader, so
// the send is also guarded with a default to stay non-blocking even if the
// waiter has already given up.
func (c *Client) deliverResult(raw []byte) {
	var res resultMsg
	if err := json.Unmarshal(raw, &res); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[res.ID]
	c.mu.Unlock()
	if ch == nil {
		return
	}
	msg := ""
	if !res.Success {
		msg = res.Error.Message
		if msg == "" {
			msg = res.Error.Code
		}
		if msg == "" {
			msg = "unknown error"
		}
	}
	select {
	case ch <- cmdResult{success: res.Success, errMsg: msg}:
	default:
	}
}

// Start runs the connection loop in a background goroutine.
func (c *Client) Start(ctx context.Context) {
	go c.loop(ctx)
}

func (c *Client) loop(ctx context.Context) {
	defer close(c.Events)
	backoff := time.Second
	for {
		start := time.Now()
		if err := c.connect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("ha: connection lost, reconnecting", "err", err, "backoff", backoff)
		}
		// A connection that lived for a while means the trouble is over;
		// don't punish the next blip for failures from last week.
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (c *Client) connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	// coder/websocket caps reads at 32 KiB by default. A full get_states
	// snapshot from a real HA install dwarfs that, so raise the limit.
	// The connection is to a trusted local Supervisor, so a generous cap
	// is fine; it only guards against a runaway message.
	conn.SetReadLimit(readLimit)

	if err := c.auth(ctx, conn); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Publish the connection only after auth: HA drops connections that
	// receive commands before authenticating, so SendRaw must not be able
	// to reach a half-open socket.
	c.mu.Lock()
	c.conn = conn
	c.subscribed = make(map[string]struct{})
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		pending := c.pending
		c.pending = make(map[int]chan cmdResult)
		c.mu.Unlock()
		// Fail every in-flight command so its waiter unblocks instead of
		// hanging until its own timeout once the connection is gone.
		for _, ch := range pending {
			select {
			case ch <- cmdResult{errMsg: "connection lost"}:
			default:
			}
		}
	}()

	// Re-seed on every connect; Tracker.Seed dedups history rows.
	states, err := c.getStates(ctx, conn)
	if err != nil {
		return fmt.Errorf("get_states: %w", err)
	}
	select {
	case <-c.States: // drop a stale batch the consumer never picked up
	default:
	}
	select {
	case c.States <- states:
	default:
	}

	// Subscribe to state_changed plus all registered extra types. Mark
	// under the lock before sending so a concurrent AddEventType cannot
	// double-subscribe the same type on this connection.
	c.mu.Lock()
	toSend := []string{"state_changed"}
	toSend = append(toSend, c.eventTypes...)
	n := 0
	for _, et := range toSend {
		if _, ok := c.subscribed[et]; ok {
			continue
		}
		c.subscribed[et] = struct{}{}
		toSend[n] = et
		n++
	}
	toSend = toSend[:n]
	c.mu.Unlock()
	for _, et := range toSend {
		if err := c.subscribe(ctx, conn, et); err != nil {
			return fmt.Errorf("subscribe %q: %w", et, err)
		}
	}

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
		if envelope.Type == "result" {
			if envelope.ID == id {
				var result getStatesResult
				if err := json.Unmarshal(raw, &result); err != nil {
					return nil, err
				}
				return result.Result, nil
			}
			// A result for some other command (e.g. a call_service issued
			// during seeding) — hand it to its waiter rather than drop it.
			c.deliverResult(raw)
		}
	}
}

func (c *Client) subscribe(ctx context.Context, conn *websocket.Conn, eventType string) error {
	return wsjson.Write(ctx, conn, subscribeMsg{
		ID:        c.nextID(),
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
		if snap.Type == "result" {
			c.deliverResult(raw)
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
