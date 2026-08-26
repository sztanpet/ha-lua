package mqtt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// connectTimeout bounds the initial connect. MQTT is optional — every other
// subsystem works without it — so a broker that is down must not hold up
// startup; paho keeps retrying in the background afterwards.
const connectTimeout = 10 * time.Second

// opTimeout bounds a subscribe or publish round trip.
const opTimeout = 5 * time.Second

// ErrDisabled is returned by every operation when no broker is configured.
var ErrDisabled = errors.New("mqtt: no broker configured")

// Config is the broker connection. Broker empty disables MQTT entirely.
type Config struct {
	Broker   string // tcp://host:1883, ssl://host:8883, ws://host:9001
	Username string
	Password string
	ClientID string
}

// Message is one inbound publication.
type Message struct {
	Topic      string
	Payload    []byte
	Retained   bool
	ReceivedAt time.Time
}

// Client is the daemon's single broker connection, shared by every script.
// Subscriptions are deduplicated across scripts (the broker gets one
// SUBSCRIBE per distinct filter) and re-sent on every reconnect; routing a
// message to the scripts that asked for it is the caller's job, which keeps
// this type ignorant of scripts entirely.
type Client struct {
	cfg       Config
	onMessage func(Message)

	// subsReady closes once the first OnConnect pass has finished sending the
	// subscriptions the broker had not seen yet. Start waits on it, so a
	// publish issued right after Start cannot beat its own SUBSCRIBE to the
	// broker and vanish.
	subsReady     chan struct{}
	subsReadyOnce sync.Once

	mu          sync.Mutex
	cli         paho.Client
	filters     map[string]struct{}
	connected   bool
	reconnects  int
	lastErr     string
	lastErrAt   time.Time
	connectedAt time.Time
}

// New builds a client. onMessage is called from paho's goroutines, once per
// inbound message, and must not block.
func New(cfg Config, onMessage func(Message)) *Client {
	return &Client{cfg: cfg, onMessage: onMessage, filters: make(map[string]struct{}),
		subsReady: make(chan struct{})}
}

// Enabled reports whether a broker is configured.
func (c *Client) Enabled() bool { return c.cfg.Broker != "" }

// Start connects to the broker. A connect failure is returned but is not
// fatal to the caller: paho retries in the background and re-subscribes
// through the OnConnect handler.
func (c *Client) Start(ctx context.Context) error {
	if !c.Enabled() {
		return ErrDisabled
	}

	opts := paho.NewClientOptions().
		AddBroker(c.cfg.Broker).
		SetClientID(c.clientID()).
		SetUsername(c.cfg.Username).
		SetPassword(c.cfg.Password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetMaxReconnectInterval(1 * time.Minute).
		SetCleanSession(true).
		SetOrderMatters(false)

	opts.SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
		c.onMessage(Message{Topic: m.Topic(), Payload: m.Payload(),
			Retained: m.Retained(), ReceivedAt: time.Now()})
	})
	// CleanSession drops the broker-side subscription state on every
	// reconnect, so restoring it here is not an optimization — without it a
	// reconnect silently stops delivering everything.
	opts.SetOnConnectHandler(func(cli paho.Client) {
		c.mu.Lock()
		c.connected = true
		c.connectedAt = time.Now()
		filters := c.filterList()
		c.mu.Unlock()

		slog.Info("mqtt: connected", "broker", c.cfg.Broker, "filters", len(filters))
		for _, f := range filters {
			if err := wait(cli.Subscribe(f, 0, nil)); err != nil {
				slog.Warn("mqtt: resubscribe failed", "filter", f, "err", err)
			}
		}
		c.subsReadyOnce.Do(func() { close(c.subsReady) })
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		c.mu.Lock()
		c.connected = false
		c.reconnects++
		c.lastErr, c.lastErrAt = err.Error(), time.Now()
		c.mu.Unlock()
		slog.Warn("mqtt: connection lost", "err", err)
	})

	cli := paho.NewClient(opts)
	c.mu.Lock()
	c.cli = cli
	c.mu.Unlock()

	token := cli.Connect()
	if !token.WaitTimeout(connectTimeout) {
		return fmt.Errorf("mqtt: connect to %s timed out", c.cfg.Broker)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt: connect to %s: %w", c.cfg.Broker, err)
	}
	select {
	case <-c.subsReady:
	case <-time.After(connectTimeout):
		slog.Warn("mqtt: subscriptions not confirmed before the connect deadline")
	case <-ctx.Done():
		return ctx.Err()
	}

	go func() {
		<-ctx.Done()
		cli.Disconnect(250)
	}()
	return nil
}

// Subscribe registers a topic filter. Idempotent: the broker sees one
// SUBSCRIBE per distinct filter no matter how many scripts ask for it.
func (c *Client) Subscribe(filter string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	if err := ValidateFilter(filter); err != nil {
		return err
	}

	c.mu.Lock()
	if _, dup := c.filters[filter]; dup {
		c.mu.Unlock()
		return nil
	}
	c.filters[filter] = struct{}{}
	cli, connected := c.cli, c.connected
	c.mu.Unlock()

	// Not connected yet: OnConnect subscribes the whole set, so recording it
	// above is all that is needed.
	if cli == nil || !connected {
		return nil
	}
	return wait(cli.Subscribe(filter, 0, nil))
}

// Publish sends payload to topic.
func (c *Client) Publish(topic string, payload []byte, qos byte, retain bool) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli == nil {
		return errors.New("mqtt: not started")
	}
	if !cli.IsConnected() {
		return errors.New("mqtt: not connected")
	}
	return wait(cli.Publish(topic, qos, retain, payload))
}

// Stats reports the link's health for the debug page.
type Stats struct {
	Broker      string    `json:"broker"`
	Connected   bool      `json:"connected"`
	ConnectedAt time.Time `json:"connected_at,omitzero"`
	Reconnects  int       `json:"reconnects"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	Filters     []string  `json:"filters"`
}

func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Stats{
		Broker:      c.cfg.Broker,
		Connected:   c.connected,
		Reconnects:  c.reconnects,
		LastError:   c.lastErr,
		LastErrorAt: c.lastErrAt,
		Filters:     c.filterList(),
	}
	if c.connected {
		st.ConnectedAt = c.connectedAt
	}
	return st
}

// filterList returns the subscribed filters, sorted. Caller holds c.mu.
func (c *Client) filterList() []string {
	out := make([]string, 0, len(c.filters))
	for f := range c.filters {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (c *Client) clientID() string {
	if c.cfg.ClientID != "" {
		return c.cfg.ClientID
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "addon"
	}
	return "ha-lua-" + host
}

func wait(t paho.Token) error {
	if !t.WaitTimeout(opTimeout) {
		return errors.New("mqtt: operation timed out")
	}
	return t.Error()
}
